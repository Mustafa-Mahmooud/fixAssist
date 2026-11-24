package projects

import (
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Per-key rate limit (FDR quotas: 50 events/s, burst 200).
const (
	DefaultRate  = 50
	DefaultBurst = 200
)

// Decision is the result of a quota check.
type Decision struct {
	Allowed    bool
	RetryAfter time.Duration
	Reason     string // "rate" or "monthly" when rejected
}

// Limits holds per-key token buckets and per-project monthly counters in
// memory. Monthly counts are seeded from Postgres on every cache refresh, so
// a restart loses at most one refresh interval of counting.
type Limits struct {
	Rate  rate.Limit
	Burst int
	Now   func() time.Time // for tests

	mu      sync.Mutex
	buckets map[string]*rate.Limiter
	month   time.Time
	usage   map[int64]int64
}

// NewLimits uses the FDR defaults.
func NewLimits() *Limits {
	return &Limits{Rate: DefaultRate, Burst: DefaultBurst}
}

func (l *Limits) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// MonthStart is the first instant of t's month in UTC.
func MonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func (l *Limits) rollMonth(now time.Time) {
	if m := MonthStart(now); !m.Equal(l.month) {
		l.month = m
		l.usage = map[int64]int64{}
	}
}

// SeedUsage merges counts from the database. Local counts can be ahead of
// the database, so the larger value wins.
func (l *Limits) SeedUsage(month time.Time, counts map[int64]int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollMonth(l.now())
	if !month.Equal(l.month) {
		return
	}
	for id, n := range counts {
		if n > l.usage[id] {
			l.usage[id] = n
		}
	}
}

// Allow checks and, if allowed, consumes n events for the project's key.
func (l *Limits) Allow(p Project, n int) Decision {
	if n <= 0 {
		return Decision{Allowed: true}
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollMonth(now)

	if p.QuotaMonthly > 0 && l.usage[p.ID]+int64(n) > p.QuotaMonthly {
		next := l.month.AddDate(0, 1, 0)
		return Decision{RetryAfter: next.Sub(now), Reason: "monthly"}
	}

	if l.buckets == nil {
		l.buckets = map[string]*rate.Limiter{}
	}
	b, ok := l.buckets[p.PublicKey]
	if !ok {
		burst := l.Burst
		if burst <= 0 {
			burst = DefaultBurst
		}
		b = rate.NewLimiter(l.Rate, burst)
		l.buckets[p.PublicKey] = b
	}
	if !b.AllowN(now, n) {
		r := b.ReserveN(now, n)
		delay := time.Second
		if r.OK() {
			delay = r.DelayFrom(now)
			r.CancelAt(now)
		}
		secs := math.Max(1, math.Ceil(delay.Seconds()))
		return Decision{RetryAfter: time.Duration(secs) * time.Second, Reason: "rate"}
	}
	l.usage[p.ID] += int64(n)
	return Decision{Allowed: true}
}

// Usage is the in-memory count for a project this month.
func (l *Limits) Usage(projectID int64) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.usage[projectID]
}

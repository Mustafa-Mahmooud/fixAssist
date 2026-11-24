package projects

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yourname/fixassist/internal/store/db"
)

// RefreshEvery is how often the in-memory key cache reloads (FDR: 30 s).
const RefreshEvery = 30 * time.Second

// negativeTTL stops unknown keys from hitting the database on every request.
const negativeTTL = 30 * time.Second

// Cache maps public keys to projects in memory, refreshed from Postgres.
// A miss falls through to one query, so new keys work immediately.
type Cache struct {
	Q      *db.Queries
	Limits *Limits // optional: seeded with this month's usage on refresh
	Log    *slog.Logger

	mu     sync.RWMutex
	byKey  map[string]Project
	misses map[string]time.Time
}

func fromRow(key string, id int64, agent, backend string, domains []string, quota int64, secret []byte) Project {
	return Project{ID: id, PublicKey: key, TargetAgent: agent, Backend: backend,
		AllowedDomains: domains, QuotaMonthly: quota, HMACSecret: secret}
}

// Refresh reloads every active key and, if Limits is set, this month's usage.
func (c *Cache) Refresh(ctx context.Context) error {
	rows, err := c.Q.ListActiveKeys(ctx)
	if err != nil {
		return err
	}
	m := make(map[string]Project, len(rows))
	for _, r := range rows {
		m[r.PublicKey] = fromRow(r.PublicKey, r.ProjectID, r.TargetAgent, r.Backend, r.AllowedDomains, r.QuotaMonthly, r.HmacSecret)
	}
	c.mu.Lock()
	c.byKey = m
	c.misses = map[string]time.Time{}
	c.mu.Unlock()

	if c.Limits != nil {
		month := MonthStart(time.Now())
		usage, err := c.Q.ListUsageForMonth(ctx, pgtype.Date{Time: month, Valid: true})
		if err != nil {
			return err
		}
		counts := make(map[int64]int64, len(usage))
		for _, u := range usage {
			counts[u.ProjectID] = u.Events
		}
		c.Limits.SeedUsage(month, counts)
	}
	return nil
}

// Run refreshes every RefreshEvery until ctx ends.
func (c *Cache) Run(ctx context.Context) {
	t := time.NewTicker(RefreshEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.Refresh(ctx); err != nil && ctx.Err() == nil {
				c.Log.Warn("project cache refresh failed", "err", err)
			}
		}
	}
}

// Lookup returns the project for an active public key.
func (c *Cache) Lookup(ctx context.Context, key string) (Project, bool, error) {
	c.mu.RLock()
	p, ok := c.byKey[key]
	missAt, missed := c.misses[key]
	c.mu.RUnlock()
	if ok {
		return p, true, nil
	}
	if missed && time.Since(missAt) < negativeTTL {
		return Project{}, false, nil
	}
	r, err := c.Q.GetActiveKey(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		c.mu.Lock()
		if c.misses == nil {
			c.misses = map[string]time.Time{}
		}
		if len(c.misses) < 100_000 {
			c.misses[key] = time.Now()
		}
		c.mu.Unlock()
		return Project{}, false, nil
	}
	if err != nil {
		return Project{}, false, err
	}
	p = fromRow(r.PublicKey, r.ProjectID, r.TargetAgent, r.Backend, r.AllowedDomains, r.QuotaMonthly, r.HmacSecret)
	c.mu.Lock()
	if c.byKey == nil {
		c.byKey = map[string]Project{}
	}
	c.byKey[key] = p
	c.mu.Unlock()
	return p, true, nil
}

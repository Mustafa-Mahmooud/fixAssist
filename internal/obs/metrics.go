// Package obs is our own observability (FDR "Observability of ourselves"):
// Prometheus metrics for ingest rate, 429s, latency, queue depth and lag,
// and job failures.
package obs

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the registry and the HTTP instruments.
type Metrics struct {
	Reg      *prometheus.Registry
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

// New registers Go/process collectors, request metrics and, when pool is
// non-nil, the River queue collector (queried on each scrape).
func New(pool *pgxpool.Pool, log *slog.Logger) *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Reg: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fix_http_requests_total", Help: "Requests by endpoint and status code (429 = rate or quota limited).",
		}, []string{"endpoint", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "fix_http_request_duration_seconds", Help: "Request latency by endpoint (FDR target: ingest p99 < 50 ms).",
			Buckets: []float64{.005, .01, .025, .05, .1, .2, .5, 1, 2.5},
		}, []string{"endpoint"}),
	}
	reg.MustRegister(m.requests, m.duration, collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	if pool != nil {
		reg.MustRegister(&queueCollector{pool: pool, log: log})
	}
	return m
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

// Wrap instruments one endpoint.
func (m *Metrics) Wrap(endpoint string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		h(rec, r)
		m.requests.WithLabelValues(endpoint, strconv.Itoa(rec.code)).Inc()
		m.duration.WithLabelValues(endpoint).Observe(time.Since(start).Seconds())
	}
}

// RequestCounts sums fix_http_requests_total by status code for endpoints
// with the given prefix (for the admin page).
func (m *Metrics) RequestCounts(prefix string) map[string]float64 {
	out := map[string]float64{}
	fams, err := m.Reg.Gather()
	if err != nil {
		return out
	}
	for _, f := range fams {
		if f.GetName() != "fix_http_requests_total" {
			continue
		}
		for _, mm := range f.GetMetric() {
			var ep, code string
			for _, l := range mm.GetLabel() {
				switch l.GetName() {
				case "endpoint":
					ep = l.GetValue()
				case "code":
					code = l.GetValue()
				}
			}
			if strings.HasPrefix(ep, prefix) {
				out[code] += mm.GetCounter().GetValue()
			}
		}
	}
	return out
}

// Handler serves /metrics to loopback callers or with "Authorization: Bearer
// <token>". Caddy also refuses /metrics from the internet.
func (m *Metrics) Handler(token string) http.Handler {
	h := promhttp.HandlerFor(m.Reg, promhttp.HandlerOpts{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(host)
		authed := token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) == 1
		if !authed && (ip == nil || !ip.IsLoopback()) {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// queueCollector reports River queue depth, lag and recent failures.
type queueCollector struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

var (
	descJobs = prometheus.NewDesc("fix_queue_jobs", "River jobs by queue and state (discarded: last 24 h).", []string{"queue", "state"}, nil)
	descLag  = prometheus.NewDesc("fix_queue_lag_seconds", "Age of the oldest available job per queue (FDR: grouping lag > 60 s means split).", []string{"queue"}, nil)
)

func (c *queueCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descJobs
	ch <- descLag
}

func (c *queueCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, err := c.pool.Query(ctx, `
		select queue, state::text, count(*) from river_job
		where state in ('available', 'running', 'retryable', 'scheduled')
		   or (state = 'discarded' and finalized_at > now() - interval '24 hours')
		group by queue, state`)
	if err != nil {
		c.log.Warn("queue metrics", "err", err)
		return
	}
	for rows.Next() {
		var q, s string
		var n int64
		if rows.Scan(&q, &s, &n) == nil {
			ch <- prometheus.MustNewConstMetric(descJobs, prometheus.GaugeValue, float64(n), q, s)
		}
	}
	rows.Close()
	rows, err = c.pool.Query(ctx, `select queue, extract(epoch from now() - min(scheduled_at))::float8 from river_job where state = 'available' group by queue`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var q string
		var lag float64
		if rows.Scan(&q, &lag) == nil {
			ch <- prometheus.MustNewConstMetric(descLag, prometheus.GaugeValue, lag, q)
		}
	}
}

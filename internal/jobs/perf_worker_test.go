package jobs_test

// Runs only with FIX_TEST_DATABASE_URL (the schema is wiped).

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertest"

	"github.com/yourname/fixassist/internal/jobs"
	"github.com/yourname/fixassist/internal/jobs/jobargs"
	"github.com/yourname/fixassist/internal/projects"
	"github.com/yourname/fixassist/internal/store"
)

// The perf job, run by River for real: a slowdown seen on two runs opens an
// issue and queues the usual analysis in the same transaction.
func TestPerfWorkerQueuesAnalysis(t *testing.T) {
	url := os.Getenv("FIX_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("FIX_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, "drop schema public cascade; create schema public"); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, pool, log); err != nil {
		t.Fatal(err)
	}
	p, err := (&projects.Service{Pool: pool}).Create(ctx, projects.CreateParams{Org: "w", Name: "w", Agent: "cursor", Backend: "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`insert into releases (project_id, release_id, first_seen_at, last_seen_at) values ($1, 'r1', $2, $2), ($1, 'r2', $3, $3)`,
		p.ID, now.AddDate(0, 0, -10), now.Add(-26*time.Hour))
	exec(`insert into daily_vitals (project_id, day, route, device, release_id, metric, bucket, n) values
		($1, current_date - 3, '/', 'mobile', 'r1', 'LCP', 16, 100),
		($1, current_date, '/', 'mobile', 'r2', 'LCP', 27, 100)`, p.ID)
	// Seen once already, over an hour ago.
	exec(`insert into perf_candidates (project_id, route, metric, first_seen_at) values ($1, '/', 'LCP', $2)`, p.ID, now.Add(-2*time.Hour))

	w := rivertest.NewWorker(t, riverpgxv5.New(pool), &river.Config{}, &jobs.PerfWorker{Pool: pool, Log: log})
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	res, err := w.Work(ctx, t, tx, jobargs.PerfCheck{}, nil)
	if err != nil {
		t.Fatalf("perf job: %v", err)
	}
	if res.EventKind != river.EventKindJobCompleted {
		t.Fatalf("perf job ended as %s", res.EventKind)
	}
	var issues, queued int
	_ = pool.QueryRow(ctx, `select count(*) from issues where project_id = $1 and kind = 'perf'`, p.ID).Scan(&issues)
	_ = pool.QueryRow(ctx, `select count(*) from river_job where kind = 'analyze_issue'`).Scan(&queued)
	if issues != 1 || queued != 1 {
		t.Errorf("issues %d, analysis jobs queued %d (want 1 and 1)", issues, queued)
	}
}

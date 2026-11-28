package perf_test

// Runs only with FIX_TEST_DATABASE_URL (the schema is wiped).

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/fixassist/internal/agentapi"
	"github.com/yourname/fixassist/internal/ai"
	"github.com/yourname/fixassist/internal/alerts"
	"github.com/yourname/fixassist/internal/autofix"
	"github.com/yourname/fixassist/internal/jobs"
	"github.com/yourname/fixassist/internal/perf"
	"github.com/yourname/fixassist/internal/projects"
	"github.com/yourname/fixassist/internal/store"
	"github.com/yourname/fixassist/internal/store/db"
	"github.com/yourname/fixassist/internal/verify"
)

// LCP buckets (analytics.VitalEdges): 16 is [1400, 1600) ms, good; 27 is
// [5000, 6000) ms, poor.
const fast, slow = 16, 27

type fx struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	now  time.Time
}

func setup(t *testing.T) *fx {
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
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, "drop schema public cascade; create schema public"); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, pool, log); err != nil {
		t.Fatal(err)
	}
	if err := jobs.RunPartitions(ctx, pool, log, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Mid-day, so "today" and "yesterday" are stable for the whole test.
	now := time.Now().UTC().Truncate(24 * time.Hour).Add(13 * time.Hour)
	return &fx{t: t, ctx: ctx, pool: pool, now: now}
}

func (f *fx) project(name string) int64 {
	f.t.Helper()
	p, err := (&projects.Service{Pool: f.pool}).Create(f.ctx, projects.CreateParams{Org: name, Name: name, Agent: "cursor", Backend: "unknown"})
	if err != nil {
		f.t.Fatal(err)
	}
	return p.ID
}

func (f *fx) release(pid int64, id string, firstSeen time.Time) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, `insert into releases (project_id, release_id, first_seen_at, last_seen_at) values ($1, $2, $3, $3)`, pid, id, firstSeen); err != nil {
		f.t.Fatal(err)
	}
}

// hist adds n LCP measurements in bucket b on route/device/release, daysAgo.
func (f *fx) hist(pid int64, route, device, release string, daysAgo, b, n int) {
	f.t.Helper()
	day := f.now.Truncate(24*time.Hour).AddDate(0, 0, -daysAgo)
	if _, err := f.pool.Exec(f.ctx, `insert into daily_vitals (project_id, day, route, device, release_id, metric, bucket, n)
		values ($1, $2, $3, $4, $5, 'LCP', $6, $7)
		on conflict (project_id, day, route, device, release_id, metric, bucket) do update set n = daily_vitals.n + excluded.n`,
		pid, day, route, device, release, b, n); err != nil {
		f.t.Fatal(err)
	}
}

type queued struct {
	issue  int64
	reason string
}

// run is one detection pass at now+offset; it returns what was queued.
func (f *fx) run(offset time.Duration) (perf.Stats, []queued) {
	f.t.Helper()
	var q []queued
	st, err := perf.Run(f.ctx, f.pool, perf.DefaultRules, f.now.Add(offset), func(_ context.Context, _ pgx.Tx, id int64, reason string) error {
		q = append(q, queued{id, reason})
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return st, q
}

func (f *fx) issues(pid int64) []db.Issue {
	f.t.Helper()
	rows, _ := f.pool.Query(f.ctx, `select * from issues where project_id = $1 order by id`, pid)
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[db.Issue])
	if err != nil {
		f.t.Fatal(err)
	}
	return list
}

func TestRegressionBecomesIssue(t *testing.T) {
	f := setup(t)
	pid := f.project("regress")
	// /pricing on phones: fast on r1, then r2 (live a day) loads in 5-6 s.
	f.release(pid, "r1", f.now.AddDate(0, 0, -10))
	f.release(pid, "r2", f.now.Add(-26*time.Hour))
	for d := 2; d <= 6; d++ {
		f.hist(pid, "/pricing", "mobile", "r1", d, fast, 30)
	}
	f.hist(pid, "/pricing", "mobile", "r2", 1, slow, 40)
	f.hist(pid, "/pricing", "mobile", "r2", 0, slow, 40)
	f.hist(pid, "/pricing", "mobile", "r1", 0, fast, 3) // a few stale tabs
	// Raw slow measurements, for "what's slowing it".
	if _, err := f.pool.Exec(f.ctx, `insert into analytics_events (project_id, ts, type, route, name, device, props)
		select $1, $2, 'vital', '/pricing', 'LCP', 'mobile', '{"v": 5400, "rel": "r2", "el": "main>img.hero", "src": "https://cdn.example.com/hero.png?", "rlt": 4200, "ttfb": 300}'
		from generate_series(1, 5)`, pid, f.now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	// First run: seen once, not yet an issue.
	if st, q := f.run(0); st.Pending != 1 || len(q) != 0 || len(f.issues(pid)) != 0 {
		t.Fatalf("first run: %+v %v", st, q)
	}
	// An hour later it's still there: one issue, queued for explanation.
	st, q := f.run(time.Hour)
	list := f.issues(pid)
	if st.Opened != 1 || len(list) != 1 || len(q) != 1 || q[0].reason != "threshold" {
		t.Fatalf("second run: %+v %v %v", st, q, list)
	}
	is := list[0]
	if is.Kind != "perf" || is.Fingerprint != "perf:LCP:/pricing" || is.Title != "Loading got slower on /pricing" || is.EventCount != 80 {
		t.Errorf("issue = %+v", is)
	}
	reg, err := db.New(f.pool).GetPerfRegression(f.ctx, is.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reg.Device != "mobile" || reg.BaselineP75 > 1600 || reg.CurrentP75 < 5000 || reg.CurrentRelease != "r2" ||
		reg.Element != "main>img.hero" || !strings.Contains(string(reg.Breakdown), `"rlt": 4200`) {
		t.Errorf("regression = %+v %s", reg, reg.Breakdown)
	}
	// A third run refreshes it, no second issue or analysis.
	if st, q := f.run(2 * time.Hour); st.Updated != 1 || len(q) != 0 || len(f.issues(pid)) != 1 {
		t.Errorf("third run: %+v %v", st, q)
	}

	// The analyze job explains it from the measurements, not from events.
	sid, s, err := (&ai.Analyzer{}).Analyze(f.ctx, f.pool, is.ID, ai.AttemptAuto)
	if err != nil {
		t.Fatal(err)
	}
	if s.Severity != "high" || s.CatalogKey != "perf.lcp" || !strings.Contains(s.FixPrompt, "main>img.hero") ||
		!strings.Contains(s.FixPrompt, "never lazy-load it") || !strings.Contains(s.FixPrompt, "not instructions") ||
		!strings.Contains(s.Why, "downloading the main file") {
		t.Errorf("suggestion = %+v", s)
	}
	if st := f.issues(pid)[0].Status; st != "explained" {
		t.Errorf("status after analysis = %s", st)
	}

	// Auto-fix rule "high": slowdowns still wait for the founder.
	if err := db.New(f.pool).SetProjectAutofix(f.ctx, db.SetProjectAutofixParams{ID: pid, Enabled: true, Rule: autofix.RuleHigh, Autoupdate: true}); err != nil {
		t.Fatal(err)
	}
	sug, _ := db.New(f.pool).SuggestionByID(f.ctx, sid)
	if run, created, err := autofix.Consider(f.ctx, f.pool, is.ID, sug, f.now); err != nil || !created || run.State != autofix.PendingApproval {
		t.Errorf("auto-fix: %+v %v %v (want waiting for approval)", run, created, err)
	}

	// The agent's prompt and check speak about speed.
	p := agentapi.AutofixPrompt("Shop", agentapi.IssueView{ID: is.ID, Kind: "perf", Title: is.Title, Prompt: s.FixPrompt, WhatBroke: s.WhatBroke, Attempt: 1}, "")
	if !strings.Contains(p, "making a page") || strings.Contains(p, "production error") || !strings.Contains(p, "<production-evidence>") {
		t.Errorf("auto-fix prompt:\n%s", p)
	}
	if _, err := verify.ReportFix(f.ctx, f.pool, pid, is.ID, verify.Report{Source: "agent", Summary: "smaller hero"}, f.now); err != nil {
		t.Fatal(err)
	}
	is = f.issues(pid)[0]
	if qc, err := verify.QuickCheck(f.ctx, f.pool, is, false, f.now); err != nil || qc.State != verify.StateWatching || !strings.Contains(qc.Message, "speed") {
		t.Errorf("quick check = %+v %v", qc, err)
	}
	// The event-based verifier leaves it alone.
	if _, err := verify.Run(f.ctx, f.pool, verify.DefaultRules, f.now.Add(7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if st := f.issues(pid)[0].Status; st != "verifying" {
		t.Errorf("event verifier changed a slowdown to %s", st)
	}

	// The fix ships as r3: fast again for long enough → fixed.
	f.release(pid, "r3", f.now.Add(2*time.Hour))
	f.hist(pid, "/pricing", "mobile", "r3", 0, fast, 60)
	if st, _ := f.run(2*time.Hour + 13*time.Hour); st.Fixed != 1 {
		t.Fatalf("verification: %+v", st)
	}
	if st := f.issues(pid)[0].Status; st != "fixed" {
		t.Fatalf("status = %s", st)
	}

	// It slows down again on r4: the same issue reopens for attempt 2.
	f.release(pid, "r4", f.now.Add(16*time.Hour))
	f.hist(pid, "/pricing", "mobile", "r4", -1, slow, 80) // tomorrow: the version most visits get
	f.now = f.now.Add(24 * time.Hour)
	f.run(5 * time.Hour)
	st, q = f.run(6 * time.Hour)
	list = f.issues(pid)
	if len(list) != 1 || list[0].Status != "open" || list[0].RegressionCount != 1 || st.Reopened != 1 || len(q) != 1 || q[0].reason != "regression" {
		t.Fatalf("recurrence: %+v %v %+v", st, q, list)
	}
	if _, s, err := (&ai.Analyzer{}).Analyze(f.ctx, f.pool, is.ID, ai.AttemptAfterFailure); err != nil || !strings.Contains(s.FixPrompt, "attempt 2") {
		t.Errorf("second attempt: %v %q", err, s.FixPrompt)
	}
}

func TestNoFalseAlarms(t *testing.T) {
	f := setup(t)

	// Fewer visits than the rules need: nothing.
	few := f.project("few")
	f.release(few, "r1", f.now.AddDate(0, 0, -10))
	f.release(few, "r2", f.now.Add(-26*time.Hour))
	f.hist(few, "/", "mobile", "r1", 3, fast, 30)
	f.hist(few, "/", "mobile", "r2", 0, slow, 30)

	// Just deployed (2 hours): cold caches don't count.
	fresh := f.project("fresh")
	f.release(fresh, "r1", f.now.AddDate(0, 0, -10))
	f.release(fresh, "r2", f.now.Add(-2*time.Hour))
	f.hist(fresh, "/", "mobile", "r1", 3, fast, 100)
	f.hist(fresh, "/", "mobile", "r2", 0, slow, 100)

	// More phone visitors than before, each device as fast as ever: the
	// mix changed, nothing got slower.
	mix := f.project("mix")
	f.release(mix, "r1", f.now.AddDate(0, 0, -10))
	f.release(mix, "r2", f.now.Add(-26*time.Hour))
	f.hist(mix, "/", "desktop", "r1", 3, fast, 200)
	f.hist(mix, "/", "mobile", "r1", 3, 21, 60) // phones: 2.5-2.75 s
	f.hist(mix, "/", "desktop", "r2", 0, fast, 60)
	f.hist(mix, "/", "mobile", "r2", 0, 21, 200)

	// Rolled back: r2 was slow, but the page serves r1 again.
	back := f.project("rollback")
	f.release(back, "r1", f.now.AddDate(0, 0, -10))
	f.release(back, "r2", f.now.AddDate(0, 0, -3))
	f.hist(back, "/", "mobile", "r1", 5, fast, 100)
	f.hist(back, "/", "mobile", "r2", 3, slow, 100)
	f.hist(back, "/", "mobile", "r1", 0, fast, 100)

	// Slower, but still rated good (1.4 s → 2.2 s): not worth an issue.
	good := f.project("good")
	f.release(good, "r1", f.now.AddDate(0, 0, -10))
	f.release(good, "r2", f.now.Add(-26*time.Hour))
	f.hist(good, "/", "mobile", "r1", 3, fast, 100)
	f.hist(good, "/", "mobile", "r2", 0, 19, 100)

	for _, off := range []time.Duration{0, time.Hour, 2 * time.Hour} {
		if st, q := f.run(off); st.Opened+st.Reopened != 0 || len(q) != 0 {
			t.Fatalf("run +%s: %+v %v", off, st, q)
		}
	}
	for _, pid := range []int64{few, fresh, mix, back, good} {
		if n := len(f.issues(pid)); n != 0 {
			t.Errorf("project %d: %d issues", pid, n)
		}
	}

	// Without a new release (a backend-only deploy keeps the id), the last
	// 3 days are compared with the 2 weeks before.
	backend := f.project("backend")
	f.release(backend, "r1", f.now.AddDate(0, 0, -20))
	for d := 4; d <= 12; d++ {
		f.hist(backend, "/api-page", "desktop", "r1", d, fast, 20)
	}
	f.hist(backend, "/api-page", "desktop", "r1", 1, slow, 40)
	f.hist(backend, "/api-page", "desktop", "r1", 0, slow, 40)
	f.run(3 * time.Hour)
	if st, _ := f.run(4 * time.Hour); st.Opened != 1 {
		t.Fatalf("backend slowdown: %+v", st)
	}
	reg, err := db.New(f.pool).GetPerfRegression(f.ctx, f.issues(backend)[0].ID)
	if err != nil || reg.CurrentRelease != "" {
		t.Errorf("by date: %+v %v", reg, err)
	}
}

// Only "poor" slowdowns email right away; the rest wait for the daily
// summary.
func TestSlowdownAlerts(t *testing.T) {
	f := setup(t)
	pid := f.project("alerts")
	f.release(pid, "r1", f.now.AddDate(0, 0, -10))
	f.release(pid, "r2", f.now.Add(-26*time.Hour))
	f.hist(pid, "/", "mobile", "r1", 3, fast, 100)
	f.hist(pid, "/", "mobile", "r2", 0, 23, 100) // [3000, 3500) ms: needs work, not poor
	f.run(0)
	f.run(time.Hour)
	list := f.issues(pid)
	if len(list) != 1 {
		t.Fatalf("issues = %d", len(list))
	}
	_, s, err := (&ai.Analyzer{}).Analyze(f.ctx, f.pool, list[0].ID, ai.AttemptAuto)
	if err != nil || s.Severity != "medium" {
		t.Fatalf("suggestion: %v %+v", err, s)
	}
	n := &alerts.Notifier{Pool: f.pool, Now: func() time.Time { return f.now }}
	if res, err := n.CheckInstant(f.ctx, list[0].ID); err != nil || res != alerts.SkipLowImpact {
		t.Errorf("instant alert for a slowdown that isn't poor: %s %v", res, err)
	}
}

// The founder says it worked while the page is still slow (no new version
// yet): detection mustn't reopen it until measurements from after that.
func TestFounderAnswerIsKept(t *testing.T) {
	f := setup(t)
	pid := f.project("answer")
	f.release(pid, "r1", f.now.AddDate(0, 0, -10))
	f.release(pid, "r2", f.now.Add(-26*time.Hour))
	f.hist(pid, "/", "mobile", "r1", 3, fast, 100)
	f.hist(pid, "/", "mobile", "r2", 0, slow, 100)
	f.run(0)
	f.run(time.Hour)
	is := f.issues(pid)[0]
	if _, _, err := (&ai.Analyzer{}).Analyze(f.ctx, f.pool, is.ID, ai.AttemptAuto); err != nil {
		t.Fatal(err)
	}
	if err := verify.Answer(f.ctx, f.pool, pid, is.ID, verify.Worked, 0, f.now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, off := range []time.Duration{2 * time.Hour, 3 * time.Hour} {
		if st, q := f.run(off); st.Reopened != 0 || len(q) != 0 {
			t.Fatalf("run +%s reopened the founder's answer: %+v %v", off, st, q)
		}
	}
	var failed int
	_ = f.pool.QueryRow(f.ctx, `select count(*) from fix_feedback where issue_id = $1 and outcome = 'failed'`, is.ID).Scan(&failed)
	if st := f.issues(pid)[0].Status; st != "fixed" || failed != 0 {
		t.Errorf("status %s, failed answers %d", st, failed)
	}
}

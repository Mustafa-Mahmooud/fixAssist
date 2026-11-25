package verify_test

// Runs only with FIX_TEST_DATABASE_URL (the schema is wiped).

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/fixassist/internal/grouping"
	"github.com/yourname/fixassist/internal/jobs"
	"github.com/yourname/fixassist/internal/projects"
	"github.com/yourname/fixassist/internal/store"
	"github.com/yourname/fixassist/internal/store/db"
	"github.com/yourname/fixassist/internal/verify"
)

type fx struct {
	t    *testing.T
	pool *pgxpool.Pool
	pid  int64
	t0   time.Time
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
	p, err := (&projects.Service{Pool: pool}).Create(ctx, projects.CreateParams{Org: "acme", Name: "app", Agent: "lovable", Backend: "supabase"})
	if err != nil {
		t.Fatal(err)
	}
	// t0 is "the fix was suggested"; it's 10 days ago so ages can be simulated
	// by passing a later `now` to verify.Run.
	return &fx{t: t, pool: pool, pid: p.ID, t0: time.Now().UTC().Add(-10 * 24 * time.Hour).Truncate(time.Second)}
}

func (f *fx) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

func (f *fx) str(sql string, args ...any) string {
	f.t.Helper()
	var s string
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return s
}

func (f *fx) release(id string, firstSeen time.Time) {
	f.exec("insert into releases (project_id, release_id, first_seen_at, last_seen_at) values ($1, $2, $3, $3)", f.pid, id, firstSeen)
}

// explainedIssue creates an explained issue on route with a suggestion at t0.
func (f *fx) explainedIssue(fp, route string) int64 { return f.explainedIssueKind(fp, route, "net") }

func (f *fx) explainedIssueKind(fp, route, kind string) int64 {
	var id int64
	err := f.pool.QueryRow(context.Background(), `insert into issues (project_id, fingerprint, kind, signature, title, route, status, first_seen_at, last_seen_at, event_count)
		values ($1, $2, $5, 'sig-'||$2, 't', $3, 'explained', $4, $4, 3) returning id`, f.pid, fp, route, f.t0.Add(-time.Hour), kind).Scan(&id)
	if err != nil {
		f.t.Fatal(err)
	}
	// As ai.Analyze does: the suggestion points at the pattern it used.
	var kb int64
	if err := f.pool.QueryRow(context.Background(), `insert into knowledge_entries (signature, error_type, explanation_template)
		values ('sig-'||$1, 'net', 'v1:x') returning id`, fp).Scan(&kb); err != nil {
		f.t.Fatal(err)
	}
	f.exec(`insert into fix_suggestions (issue_id, origin, prompt_version, target_agent, severity, title, what_broke, who_is_affected, fix_prompt, how_to_check, created_at, kb_entry_id)
		values ($1, 'catalog', 'v1', 'lovable', 'high', 't', 'w', 'w', 'p', 'c', $2, $3)`, id, f.t0, kb)
	return id
}

func (f *fx) sessions(route, release string, n int) {
	for i := 0; i < n; i++ {
		f.exec("insert into pageviews (project_id, route, session_id, release_id) values ($1, $2, $3, $4)", f.pid, route, fmt.Sprintf("s%d", i), release)
	}
}

func (f *fx) run(now time.Time) verify.Stats {
	f.t.Helper()
	st, err := verify.Run(context.Background(), f.pool, verify.DefaultRules, now)
	if err != nil {
		f.t.Fatal(err)
	}
	return st
}

func TestImplicitVerification(t *testing.T) {
	f := setup(t)
	f.release("r1", f.t0.Add(-2*time.Hour)) // the release the bug was seen on
	fixed := f.explainedIssue("fp-fixed", "/signup")
	quiet := f.explainedIssue("fp-quiet", "/settings")

	// No new release yet: nothing happens.
	if st := f.run(f.t0.Add(time.Hour)); st.StartedVerifying != 0 {
		t.Fatalf("started without a new release: %+v", st)
	}

	// Rule 1: a release first seen after the suggestion starts verification.
	r2 := f.t0.Add(3 * time.Hour)
	f.release("r2", r2)
	if st := f.run(r2.Add(time.Minute)); st.StartedVerifying != 2 {
		t.Fatalf("StartedVerifying = %d, want 2", st.StartedVerifying)
	}
	if s := f.str("select status || ':' || verify_release_id from issues where id = $1", fixed); s != "verifying:r2" {
		t.Fatalf("status = %s", s)
	}

	// Rule 2: 20 sessions on /signup with r2 but only 10 h old: not yet.
	f.sessions("/signup", "r2", 20)
	f.sessions("/settings", "r2", 3)
	if st := f.run(r2.Add(10 * time.Hour)); st.VerifiedFixed != 0 {
		t.Fatalf("fixed before 48 h: %+v", st)
	}
	// 49 h later: fixed, with implicit "worked" feedback and knowledge updated.
	if st := f.run(r2.Add(49 * time.Hour)); st.VerifiedFixed != 1 {
		t.Fatalf("VerifiedFixed = %d, want 1", st.VerifiedFixed)
	}
	if s := f.str("select status || ':' || fixed_release_id from issues where id = $1", fixed); s != "fixed:r2" {
		t.Errorf("status = %s", s)
	}
	if s := f.str("select outcome || '/' || source || '/' || release_id from fix_feedback where issue_id = $1", fixed); s != "worked/implicit/r2" {
		t.Errorf("feedback = %s", s)
	}
	if s := f.str("select worked || '/' || failed || '/' || confidence from knowledge_entries where signature = 'sig-fp-fixed'"); s != "1/0/0.667" {
		t.Errorf("knowledge = %s, want 1/0/0.667 ((1+1)/(1+0+2))", s)
	}

	// Rule 4: /settings has only 3 sessions; after 7 days it's "probably fixed",
	// with no implicit feedback written.
	if st := f.run(r2.Add(8 * 24 * time.Hour)); st.ProbablyFixed != 1 {
		t.Errorf("ProbablyFixed = %d, want 1", st.ProbablyFixed)
	}
	if s := f.str("select status || ':' || (probably_fixed_at is not null) from issues where id = $1", quiet); s != "verifying:true" {
		t.Errorf("quiet issue = %s", s)
	}
	if n := f.str("select count(*)::text from fix_feedback where issue_id = $1", quiet); n != "0" {
		t.Errorf("low-traffic issue got %s feedback rows", n)
	}
}

func TestRecurrenceRespectsWatchedRelease(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.release("r1", f.t0.Add(-2*time.Hour))
	id := f.explainedIssue("fp", "/")
	f.release("r2", f.t0.Add(3*time.Hour))
	f.run(f.t0.Add(4 * time.Hour)) // verifying r2

	group := func(release string) {
		t.Helper()
		var eid int64
		var at time.Time
		err := f.pool.QueryRow(ctx, `insert into events (project_id, received_at, kind, fingerprint, release_id, route)
			values ($1, now(), 'net', 'fp', $2, '/') returning id, received_at`, f.pid, release).Scan(&eid, &at)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := grouping.GroupEvent(ctx, f.pool, eid, at, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	// A stale tab still on r1 hits it: the fix isn't judged on old code.
	group("r1")
	if s := f.str("select status from issues where id = $1", id); s != "verifying" {
		t.Fatalf("old-release event changed status to %s", s)
	}
	// The watched release hits it: the fix didn't work.
	group("r2")
	if s := f.str("select status || ':' || regression_count from issues where id = $1", id); s != "open:1" {
		t.Fatalf("status = %s, want open:1", s)
	}
	if s := f.str("select outcome || '/' || source from fix_feedback where issue_id = $1", id); s != "failed/implicit" {
		t.Errorf("feedback = %s", s)
	}
	if s := f.str("select worked || '/' || failed || '/' || confidence from knowledge_entries where signature = 'sig-fp'"); s != "0/1/0.333" {
		t.Errorf("knowledge = %s", s)
	}
}

// groupServer stores and groups one server event (as ingest + the job do).
func (f *fx) groupServer(fp, release string) {
	f.t.Helper()
	ctx := context.Background()
	var eid int64
	var at time.Time
	err := f.pool.QueryRow(ctx, `insert into events (project_id, received_at, kind, fingerprint, release_id, route)
		values ($1, now(), 'server', $2, $3, '/orders') returning id, received_at`, f.pid, fp, release).Scan(&eid, &at)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := grouping.GroupEvent(ctx, f.pool, eid, at, time.Now()); err != nil {
		f.t.Fatal(err)
	}
}

// Server fixes are verified by server deploys only, and by time when the
// server's release name never changes.
func TestServerVerification(t *testing.T) {
	f := setup(t)
	f.release("r1", f.t0.Add(-2*time.Hour))
	f.release("server:api@1", f.t0.Add(-2*time.Hour))
	byDeploy := f.explainedIssueKind("fp-deploy", "/orders", "server")

	// A frontend release doesn't verify a server fix.
	f.release("r2", f.t0.Add(3*time.Hour))
	if st := f.run(f.t0.Add(3*time.Hour + time.Minute)); st.StartedVerifying != 0 {
		t.Fatalf("a browser release started a server verification: %+v", st)
	}
	// A server deploy does.
	deploy := f.t0.Add(4 * time.Hour)
	f.release("server:api@2", deploy)
	if st := f.run(deploy.Add(time.Minute)); st.StartedVerifying != 1 {
		t.Fatalf("StartedVerifying = %d, want 1", st.StartedVerifying)
	}
	if s := f.str("select status || ':' || verify_release_id from issues where id = $1", byDeploy); s != "verifying:server:api@2" {
		t.Fatalf("status = %s", s)
	}
	// Page traffic carries the frontend release (r2): counted by time instead.
	f.sessions("/orders", "r2", 20)
	if st := f.run(deploy.Add(49 * time.Hour)); st.VerifiedFixed != 1 {
		t.Fatalf("VerifiedFixed = %d, want 1", st.VerifiedFixed)
	}

	// "I applied the fix" on a server whose release name never changes:
	// verified by time, without waiting for a new release.
	ctx := context.Background()
	manual := f.explainedIssueKind("fp-manual", "/orders", "server")
	applied := f.t0.Add(5 * time.Hour)
	if err := verify.Applied(ctx, f.pool, f.pid, manual, applied); err != nil {
		t.Fatal(err)
	}
	if st := f.run(applied.Add(49 * time.Hour)); st.VerifiedFixed != 1 {
		t.Fatalf("manual: VerifiedFixed = %d, want 1", st.VerifiedFixed)
	}

	// It happens again on the same (unchanged) release: it came back.
	f.groupServer("fp-manual", "server:api@2")
	if s := f.str("select status || ':' || regression_count from issues where id = $1", manual); s != "open:1" {
		t.Errorf("recurrence on an unchanged release: %s, want open:1", s)
	}

	// Rolling deploy: an old instance (api@1) still running after the fix
	// went out on api@2 doesn't reopen it.
	if s := f.str("select status from issues where id = $1", byDeploy); s != "fixed" {
		t.Fatalf("byDeploy = %s", s)
	}
	f.groupServer("fp-deploy", "server:api@1")
	if s := f.str("select status from issues where id = $1", byDeploy); s != "fixed" {
		t.Errorf("an old instance reopened the fix: %s", s)
	}
	// The new release hitting it does.
	f.groupServer("fp-deploy", "server:api@2")
	if s := f.str("select status from issues where id = $1", byDeploy); s != "open" {
		t.Errorf("recurrence on the new release: %s, want open", s)
	}
}

// The quick check on a browser issue: the fixed version is the next site
// release; three sessions on the page without the error look fixed.
func TestQuickCheckBrowser(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.release("r1", f.t0.Add(-2*time.Hour))
	id := f.explainedIssue("fp-qc", "/checkout")
	now := time.Now()
	if n, err := verify.ReportFix(ctx, f.pool, f.pid, id, verify.Report{Source: "agent", Summary: "guard the null cart"}, now); err != nil || n != 1 {
		t.Fatalf("ReportFix = %d, %v", n, err)
	}
	check := func(want string) verify.Quick {
		t.Helper()
		is, err := db.New(f.pool).GetIssue(ctx, db.GetIssueParams{ID: id, ProjectID: f.pid})
		if err != nil {
			t.Fatal(err)
		}
		q, err := verify.QuickCheck(ctx, f.pool, is, false, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if q.State != want {
			t.Fatalf("state = %s, want %s (%s)", q.State, want, q.Message)
		}
		return q
	}
	check(verify.StateWaitDeploy) // the site hasn't been republished
	f.release("r2", now.Add(time.Second))
	if q := check(verify.StateWatching); q.Release != "r2" {
		t.Errorf("watching release %s, want r2", q.Release)
	}
	f.sessions("/checkout", "r2", verify.QuickMinSessions)
	check(verify.StateLooksFixed)
	// An old tab on r1 hitting it doesn't count; r2 hitting it does.
	f.exec("insert into events (project_id, kind, fingerprint, release_id, route) values ($1, 'js', 'fp-qc', 'r1', '/checkout')", f.pid)
	check(verify.StateLooksFixed)
	f.exec("insert into events (project_id, kind, fingerprint, release_id, route) values ($1, 'js', 'fp-qc', 'r2', '/checkout')", f.pid)
	check(verify.StateCameBack)
}

func TestExplicitAnswersAndApplied(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.release("r1", f.t0)
	id := f.explainedIssue("fp", "/")

	// "I applied the fix": verifying, waiting for the next release.
	if err := verify.Applied(ctx, f.pool, f.pid, id, f.t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if s := f.str("select status || ':' || coalesce(verify_release_id, '-') from issues where id = $1", id); s != "verifying:-" {
		t.Fatalf("after applied: %s", s)
	}
	f.release("r2", f.t0.Add(2*time.Hour))
	f.run(f.t0.Add(3 * time.Hour))
	if s := f.str("select verify_release_id from issues where id = $1", id); s != "r2" {
		t.Errorf("watched release = %s, want r2", s)
	}

	// "Didn't work" → open; "Worked" → fixed in the latest release.
	if err := verify.Answer(ctx, f.pool, f.pid, id, verify.Failed, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if s := f.str("select status from issues where id = $1", id); s != "open" {
		t.Errorf("after failed: %s", s)
	}
	if err := verify.Answer(ctx, f.pool, f.pid, id, verify.Worked, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if s := f.str("select status || ':' || fixed_release_id from issues where id = $1", id); s != "fixed:r2" {
		t.Errorf("after worked: %s", s)
	}
	if s := f.str("select string_agg(outcome || '/' || source, ',' order by id) from fix_feedback where issue_id = $1", id); s != "failed/explicit,worked/explicit" {
		t.Errorf("feedback = %s", s)
	}
	if err := verify.Answer(ctx, f.pool, f.pid, id, "maybe", 0, time.Now()); err == nil {
		t.Error("bad outcome accepted")
	}
}

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
func (f *fx) explainedIssue(fp, route string) int64 {
	var id int64
	err := f.pool.QueryRow(context.Background(), `insert into issues (project_id, fingerprint, kind, signature, title, route, status, first_seen_at, last_seen_at, event_count)
		values ($1, $2, 'net', 'sig-'||$2, 't', $3, 'explained', $4, $4, 3) returning id`, f.pid, fp, route, f.t0.Add(-time.Hour)).Scan(&id)
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

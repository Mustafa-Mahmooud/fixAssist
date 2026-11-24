package ai_test

// Runs only with FIX_TEST_DATABASE_URL (the schema is wiped).

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/fixassist/internal/ai"
	"github.com/yourname/fixassist/internal/projects"
	"github.com/yourname/fixassist/internal/store"
	"github.com/yourname/fixassist/internal/store/db"
	"github.com/yourname/fixassist/internal/verify"
)

func kbSetup(t *testing.T) *pgxpool.Pool {
	url := os.Getenv("FIX_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("FIX_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, "drop schema public cascade; create schema public"); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, pool, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	return pool
}

func newProject(t *testing.T, pool *pgxpool.Pool, name, agent string) int64 {
	p, err := (&projects.Service{Pool: pool}).Create(context.Background(), projects.CreateParams{Org: name, Name: name, Agent: agent, Backend: "supabase"})
	if err != nil {
		t.Fatal(err)
	}
	return p.ID
}

// netIssue creates an issue (with one stored sample) for a PostgREST error.
func netIssue(t *testing.T, pool *pgxpool.Pool, projectID int64, agent, code, message string) int64 {
	t.Helper()
	body := fmt.Sprintf(`{"code":%q,"details":null,"hint":null,"message":%q}`, code, message)
	payload := fmt.Sprintf(`{"exception":{"values":[{"type":"HTTPError","value":"POST /rest/v1/profiles 400"}]},"request":{"url":"https://app.example.com/signup"},
		"contexts":{"fix_http":{"method":"POST","url":"https://x.supabase.co/rest/v1/profiles","status":400,"body":%q}}}`, body)
	sig := fmt.Sprintf(`net POST /rest/v1/profiles 400 pg:%s %q platform=%s stack=supabase`, code, message, agent)
	var id int64
	ctx := context.Background()
	err := pool.QueryRow(ctx, `insert into issues (project_id, fingerprint, kind, signature, title, route, first_seen_at, last_seen_at, event_count, user_count)
		values ($1, $2, 'net', $3, 't', '/signup', now(), now(), 3, 2) returning id`, projectID, fmt.Sprintf("fp-%d-%s-%d", projectID, code, time.Now().UnixNano()), sig).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into issue_samples (issue_id, event_id, received_at, payload) values ($1, 1, now(), $2)`, id, payload); err != nil {
		t.Fatal(err)
	}
	return id
}

func answer(t *testing.T, pool *pgxpool.Pool, issueID int64, outcome string) {
	t.Helper()
	q := db.New(pool)
	if err := verify.Record(context.Background(), q, issueID, outcome, verify.Explicit, "", 0); err != nil {
		t.Fatal(err)
	}
}

func TestKnowledgeReuseAcrossApps(t *testing.T) {
	pool := kbSetup(t)
	ctx := context.Background()
	msg := "value too long for type character varying(20)"
	a, b, c := newProject(t, pool, "a", "lovable"), newProject(t, pool, "b", "cursor"), newProject(t, pool, "c", "bolt")

	ia := netIssue(t, pool, a, "lovable", "22001", msg)
	ib := netIssue(t, pool, b, "cursor", "22001", msg)
	_, sa, err := ai.Analyze(ctx, pool, ia, 1)
	if err != nil || sa.Origin != "catalog" {
		t.Fatalf("first app: %+v %v", sa.Origin, err)
	}
	if _, _, err := ai.Analyze(ctx, pool, ib, 1); err != nil {
		t.Fatal(err)
	}
	// One pattern across apps (platform tags stripped), seen by 2 apps.
	var entries, apps int
	_ = pool.QueryRow(ctx, "select count(*) from knowledge_entries").Scan(&entries)
	_ = pool.QueryRow(ctx, "select count(*) from kb_sightings").Scan(&apps)
	if entries != 1 || apps != 2 {
		t.Fatalf("entries = %d, sightings = %d; want 1 and 2", entries, apps)
	}
	var sig string
	_ = pool.QueryRow(ctx, "select signature from knowledge_entries").Scan(&sig)
	if strings.Contains(sig, "platform=") || strings.Contains(sig, "stack=") {
		t.Errorf("kb signature keeps per-app tags: %s", sig)
	}

	// Both apps say it worked → proven; a third app gets it as a reused fix.
	answer(t, pool, ia, verify.Worked)
	answer(t, pool, ib, verify.Worked)
	ic := netIssue(t, pool, c, "bolt", "22001", msg)
	_, sc, err := ai.Analyze(ctx, pool, ic, 1)
	if err != nil || sc.Origin != "cache" {
		t.Fatalf("third app origin = %s, %v; want cache (proven)", sc.Origin, err)
	}
	var rec db.KBTrackRecordRow
	var kbID int64
	_ = pool.QueryRow(ctx, "select kb_entry_id from fix_suggestions where issue_id = $1", ic).Scan(&kbID)
	rec, err = db.New(pool).KBTrackRecord(ctx, kbID)
	if err != nil || rec.Worked != 2 || rec.Apps != 3 || rec.Confidence != 0.75 {
		t.Errorf("track record = %+v, %v", rec, err)
	}

	// Similar (not identical) errors elsewhere: aggregates only.
	sim, err := db.New(pool).KBSimilar(ctx, db.KBSimilarParams{
		Signature: `net POST /rest/v1/members 400 pg:22001 "value too long for type character varying(40)" platform=v0`, MinSimilarity: ai.SimilarMinScore})
	if err != nil || sim.Entries != 1 || sim.Worked != 2 {
		t.Errorf("similar = %+v, %v", sim, err)
	}
}

func TestQuarantineFallsBackToDiagnosis(t *testing.T) {
	pool := kbSetup(t)
	ctx := context.Background()
	msg := "new row violates row-level security policy for table \"secrets\""
	var last int64
	for i := 0; i < verify.QuarantineMinAnswers; i++ {
		p := newProject(t, pool, fmt.Sprintf("p%d", i), "lovable")
		last = netIssue(t, pool, p, "lovable", "42501", msg)
		if _, s, err := ai.Analyze(ctx, pool, last, 1); err != nil || s.Origin != "catalog" {
			t.Fatalf("app %d: %s %v", i, s.Origin, err)
		}
		answer(t, pool, last, verify.Failed)
	}
	var quarantined bool
	var conf float64
	_ = pool.QueryRow(ctx, "select quarantined_at is not null, confidence::float8 from knowledge_entries").Scan(&quarantined, &conf)
	if !quarantined || conf >= 0.3 {
		t.Fatalf("after 5 failures: quarantined=%v confidence=%v", quarantined, conf)
	}

	// The next app gets the diagnose-in-your-code fallback, not the failing fix.
	p := newProject(t, pool, "next", "lovable")
	is := netIssue(t, pool, p, "lovable", "42501", msg)
	_, s, err := ai.Analyze(ctx, pool, is, 1)
	if err != nil {
		t.Fatal(err)
	}
	if s.Origin != "agent" || !strings.HasPrefix(s.FixPrompt, "The standard fix for this error didn't work") {
		t.Errorf("fallback = %s: %.80q", s.Origin, s.FixPrompt)
	}
	var kb *int64
	_ = pool.QueryRow(ctx, "select kb_entry_id from fix_suggestions where issue_id = $1", is).Scan(&kb)
	if kb != nil {
		t.Error("fallback suggestion must not point at the quarantined pattern")
	}
	// Answers on the fallback don't move the quarantined pattern.
	answer(t, pool, is, verify.Worked)
	var worked int
	_ = pool.QueryRow(ctx, "select worked from knowledge_entries").Scan(&worked)
	if worked != 0 {
		t.Errorf("fallback answer counted against the pattern (worked=%d)", worked)
	}
}

package grouping_test

// Runs only with FIX_TEST_DATABASE_URL (the schema is wiped). See
// internal/ingest/pipeline_test.go.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/fixassist/internal/grouping"
	"github.com/yourname/fixassist/internal/ingest"
	"github.com/yourname/fixassist/internal/jobs"
	"github.com/yourname/fixassist/internal/projects"
	"github.com/yourname/fixassist/internal/store"
	"github.com/yourname/fixassist/internal/store/db"
)

func TestRouteWeightAndImpact(t *testing.T) {
	for route, want := range map[string]float64{"/checkout": 3, "/sign-up": 3, "/login": 3, "/auth/signin": 3, "/": 1, "/dashboard": 1, "/carton": 1} {
		if got := grouping.RouteWeight(route, nil); got != want {
			t.Errorf("RouteWeight(%q) = %v, want %v", route, got, want)
		}
	}
	if grouping.RouteWeight("/checkout", []string{"/pricing"}) != 1 || grouping.RouteWeight("/pricing/annual", []string{"/pricing"}) != 3 {
		t.Error("explicit key pages should replace the guess")
	}
	if grouping.Impact(0, "/", nil) != 1 || grouping.Impact(2, "/checkout", nil) != 6 {
		t.Error("impact = max(users,1) × weight")
	}
}

type golden struct {
	body    []byte
	kase    string
	release string
}

func loadGolden(t *testing.T) []golden {
	files, _ := filepath.Glob("../ingest/testdata/envelopes/*.envelope")
	sort.Strings(files)
	if len(files) == 0 {
		t.Skip("no golden envelopes")
	}
	var out []golden
	for _, f := range files {
		b, _ := os.ReadFile(f)
		env, err := ingest.ParseEnvelope(b)
		if err != nil {
			t.Fatal(err)
		}
		evs, _ := env.Events()
		out = append(out, golden{body: b, kase: evs[0].Tags["spike_case"], release: evs[0].Release})
	}
	return out
}

type fixture struct {
	pool    *pgxpool.Pool
	srv     *httptest.Server
	project projects.Project
}

func setup(t *testing.T) *fixture {
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
	p, err := (&projects.Service{Pool: pool}).Create(ctx, projects.CreateParams{Org: "acme", Name: "spike", Agent: "lovable", Backend: "supabase"})
	if err != nil {
		t.Fatal(err)
	}
	rc, err := jobs.NewClient(pool, log, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = rc.Stop(c)
	})
	cache := &projects.Cache{Q: db.New(pool), Log: log}
	if err := cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&ingest.Handler{Sink: &ingest.PGSink{Pool: pool, River: rc}, Log: log, Projects: cache}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &fixture{pool: pool, srv: srv, project: p}
}

func (f *fixture) send(t *testing.T, body []byte) {
	t.Helper()
	url := fmt.Sprintf("%s/api/%d/envelope/?sentry_key=%s", f.srv.URL, f.project.ID, f.project.PublicKey)
	resp, err := http.Post(url, "text/plain", strings.NewReader(string(body)))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("send: %v %v", err, resp)
	}
	resp.Body.Close()
}

func (f *fixture) waitGrouped(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var left int
		_ = f.pool.QueryRow(context.Background(), "select count(*) from events where grouped_at is null").Scan(&left)
		if left == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d events still ungrouped", left)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func scalar[T any](t *testing.T, pool *pgxpool.Pool, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return v
}

func TestGroupingGoldenEnvelopes(t *testing.T) {
	f := setup(t)
	g := loadGolden(t)
	for _, e := range g {
		f.send(t, e.body)
	}
	f.waitGrouped(t)

	if n := scalar[int64](t, f.pool, "select count(*) from issues"); n != 8 {
		t.Fatalf("issues = %d, want 8 (one per case)", n)
	}
	// Every case was hit once per build by one tab per build: 2 events, 2 users.
	rows, _ := f.pool.Query(context.Background(), "select kind, title, event_count, user_count, impact_score::float8, route from issues order by id")
	for rows.Next() {
		var kind, title, route string
		var events, users int64
		var impact float64
		_ = rows.Scan(&kind, &title, &events, &users, &impact, &route)
		t.Logf("%-4s events=%d users=%d impact=%.0f %s", kind, events, users, impact, title)
		if events != 2 || users != 2 || impact != 2 || route != "/" {
			t.Errorf("issue %q: events=%d users=%d impact=%v route=%q", title, events, users, impact, route)
		}
	}
	rows.Close()

	for _, want := range []string{
		"TypeError: Cannot read properties of undefined (reading 'profile')",
		"POST /rest/v1/profiles returned 400: value too long for type character varying(20)",
		"POST /auth/v1/token returned 400: Invalid login credentials",
		"GET /ping failed: Failed to fetch",
	} {
		if n := scalar[int64](t, f.pool, "select count(*) from issues where title = $1", want); n != 1 {
			t.Errorf("missing issue title %q", want)
		}
	}
	if n := scalar[int64](t, f.pool, "select count(*) from issues where signature like 'net POST /rest/v1/profiles 400 pg:22001%platform=lovable stack=supabase'"); n != 1 {
		t.Error("network signature missing project tags or code")
	}
	if n := scalar[int64](t, f.pool, "select count(*) from releases"); n != 2 {
		t.Errorf("releases = %d, want 2", n)
	}
	if n := scalar[int64](t, f.pool, "select count(*) from issue_samples"); n != 16 {
		t.Errorf("samples = %d, want 16", n)
	}
	if n := scalar[int64](t, f.pool, "select coalesce(sum(events),0) from issue_hourly"); n != 16 {
		t.Errorf("hourly events = %d, want 16", n)
	}
	if n := scalar[int64](t, f.pool, "select count(*) from events where issue_id is null"); n != 0 {
		t.Errorf("%d events without issue_id", n)
	}
	if n := scalar[int64](t, f.pool, "select count(distinct first_release_id) from issues"); n != 1 {
		t.Errorf("first_release_id should be build 1 for all issues")
	}
}

func TestGroupingIdempotentAndRegression(t *testing.T) {
	f := setup(t)
	g := loadGolden(t)
	var b1, b2 *golden
	for i := range g {
		if g[i].kase == "js_typeerror" {
			if b1 == nil {
				b1 = &g[i]
			} else if g[i].release != b1.release {
				b2 = &g[i]
			}
		}
	}
	if b1 == nil || b2 == nil {
		t.Fatal("need js_typeerror from both builds")
	}
	ctx := context.Background()
	f.send(t, b1.body)
	f.waitGrouped(t)

	// A retried job must not count twice.
	var id int64
	var at time.Time
	_ = f.pool.QueryRow(ctx, "select id, received_at from events limit 1").Scan(&id, &at)
	res, err := grouping.GroupEvent(ctx, f.pool, id, at, time.Now())
	if err != nil || !res.Skipped {
		t.Fatalf("regroup: %+v %v", res, err)
	}
	issueID := scalar[int64](t, f.pool, "select id from issues")
	if n := scalar[int64](t, f.pool, "select event_count from issues"); n != 1 {
		t.Errorf("event_count = %d after retry, want 1", n)
	}

	// Fixed in build 1; the same build again does not reopen it.
	q := db.New(f.pool)
	if _, err := q.MarkIssueFixed(ctx, db.MarkIssueFixedParams{ID: issueID, ProjectID: f.project.ID, ReleaseID: pgText(b1.release)}); err != nil {
		t.Fatal(err)
	}
	f.send(t, b1.body)
	f.waitGrouped(t)
	if s := scalar[string](t, f.pool, "select status from issues"); s != "fixed" {
		t.Errorf("status = %s after same-release event, want fixed", s)
	}
	// Seen on build 2: regression.
	f.send(t, b2.body)
	f.waitGrouped(t)
	if s := scalar[string](t, f.pool, "select status || ':' || regression_count from issues"); s != "open:1" {
		t.Errorf("status = %s, want open:1", s)
	}
	// Third event, still one user: analysis threshold reached by events (3).
	if n := scalar[int64](t, f.pool, "select event_count from issues"); n != 3 {
		t.Errorf("event_count = %d", n)
	}
}

func pgText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

// A project's first issues are explained after one event; later ones wait
// for the threshold, and an early one isn't explained again when it gets there.
func TestEarlyExplain(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	byCase := map[string][]golden{}
	var cases []string
	for _, g := range loadGolden(t) {
		if byCase[g.kase] == nil {
			cases = append(cases, g.kase)
		}
		byCase[g.kase] = append(byCase[g.kase], g)
	}
	if len(cases) <= grouping.EarlyExplainIssues {
		t.Fatalf("need more than %d distinct golden cases, have %d", grouping.EarlyExplainIssues, len(cases))
	}
	queued := func() int64 {
		return scalar[int64](t, f.pool, "select count(distinct (args->>'issue_id')) from river_job where kind = 'analyze_issue'")
	}
	for _, c := range cases[:grouping.EarlyExplainIssues+1] {
		f.send(t, byCase[c][0].body)
		f.waitGrouped(t)
	}
	if n := scalar[int64](t, f.pool, "select count(*) from issues"); n != int64(grouping.EarlyExplainIssues+1) {
		t.Fatalf("issues = %d", n)
	}
	if got := queued(); got != int64(grouping.EarlyExplainIssues) {
		t.Errorf("issues queued for explanation after one event = %d, want %d", got, grouping.EarlyExplainIssues)
	}
	last := scalar[int64](t, f.pool, "select max(id) from issues")
	if n := scalar[int64](t, f.pool, "select count(*) from river_job where kind = 'analyze_issue' and (args->>'issue_id')::bigint = $1", last); n != 0 {
		t.Error("the 4th issue must wait for the threshold")
	}

	// The first issue reaches the threshold (3 events). With the earlier
	// jobs gone River can't dedupe, so only the early rule keeps it quiet.
	if _, err := f.pool.Exec(ctx, "delete from river_job where kind = 'analyze_issue'"); err != nil {
		t.Fatal(err)
	}
	first := byCase[cases[0]][0].body
	f.send(t, first)
	f.send(t, first)
	f.waitGrouped(t)
	if n := scalar[int64](t, f.pool, "select event_count from issues order by id limit 1"); n < int64(grouping.AnalyzeMinEvents) {
		t.Fatalf("first issue has %d events", n)
	}
	if got := queued(); got != 0 {
		t.Errorf("early issue explained again at the threshold (%d jobs)", got)
	}
}

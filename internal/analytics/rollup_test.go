package analytics_test

// Runs only with FIX_TEST_DATABASE_URL (the schema is wiped).

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/fixassist/internal/analytics"
	"github.com/yourname/fixassist/internal/ingest"
	"github.com/yourname/fixassist/internal/jobs"
	"github.com/yourname/fixassist/internal/projects"
	"github.com/yourname/fixassist/internal/store"
	"github.com/yourname/fixassist/internal/store/db"
)

type fx struct {
	t    *testing.T
	pool *pgxpool.Pool
	srv  *httptest.Server
	p    projects.Project
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
	cache := &projects.Cache{Q: db.New(pool), Log: log}
	if err := cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	rc, err := jobs.NewClient(pool, log, false)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&ingest.Handler{Sink: &ingest.PGSink{Pool: pool, River: rc}, Log: log, Projects: cache,
		Enrich: &analytics.Enricher{Secret: []byte("test-secret")}}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &fx{t: t, pool: pool, srv: srv, p: p}
}

// visit sends what the SDK sends for one browser tab.
func (f *fx) visit(ua, sid, device string, pages []string, events string) {
	f.t.Helper()
	post := func(path, body string) {
		req, _ := http.NewRequest("POST", f.srv.URL+path+"?sentry_key="+f.p.PublicKey, strings.NewReader(body))
		req.Header.Set("User-Agent", ua)
		req.Header.Set("Origin", "https://app.example.com")
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 204 {
			f.t.Fatalf("%s: %v %v", path, err, resp)
		}
		resp.Body.Close()
	}
	for i, pg := range pages {
		load := 0
		if i == 0 {
			load = 1200
		}
		post("/v1/pv", fmt.Sprintf(`{"path":%q,"session_id":%q,"device":%q,"load_ms":%d,"release":"r1"}`, pg, sid, device, load))
	}
	if events != "" {
		post("/v1/a", fmt.Sprintf(`{"session_id":%q,"device":%q,"events":[%s]}`, sid, device, events))
	}
}

func (f *fx) val(sql string, args ...any) string {
	f.t.Helper()
	var s string
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return s
}

func TestCollectAndRollup(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, err := f.pool.Exec(ctx, `insert into funnels (project_id, name, steps) values ($1, 'Signup', $2)`, f.p.ID,
		`[{"kind":"page","match":"/"},{"kind":"page","match":"/signup"},{"kind":"signup","match":""}]`)
	if err != nil {
		t.Fatal(err)
	}

	f.visit("UA-1", "s1", "desktop", []string{"/", "/signup"},
		`{"type":"session_start","path":"/","referrer":"https://www.google.com/search?q=x"},
		 {"type":"click","path":"/","name":"Get started","props":{"tag":"button"}},
		 {"type":"signup","path":"/signup"},
		 {"type":"identify","path":"/signup","account":"user-42"}`)
	f.visit("UA-2", "s2", "mobile", []string{"/", "/pricing"},
		`{"type":"session_start","path":"/","referrer":"","utm":{"utm_source":"Newsletter","utm_campaign":"nov"}},
		 {"type":"bogus","path":"/"}`)
	f.visit("UA-3", "s3", "mobile", []string{"/"},
		`{"type":"session_start","path":"/","referrer":"https://app.example.com/other"},
		 {"type":"custom","path":"/","name":"video_played","props":{"seconds":12,"email":"jane@example.com","nested":{"x":1}}}`)

	// Raw rows: visitor hash everywhere, account stored only as a hash.
	if n := f.val("select count(*)::text from pageviews where visitor_hash like 'd:%' and device <> ''"); n != "5" {
		t.Errorf("enriched page views = %s, want 5", n)
	}
	if n := f.val("select count(*)::text from analytics_events"); n != "7" {
		t.Errorf("analytics events = %s, want 7 (unknown type dropped)", n)
	}
	if s := f.val("select account_hash from analytics_events where type = 'identify'"); len(s) != 32 || strings.Contains(s, "user-42") {
		t.Errorf("account hash = %q", s)
	}
	if s := f.val("select props::text from analytics_events where type = 'custom'"); s != `{"email": "[email]", "seconds": 12}` {
		t.Errorf("props = %s (nested dropped, email scrubbed)", s)
	}

	if err := analytics.RollupDay(ctx, f.pool, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Rerunning gives the same numbers (idempotent).
	if err := analytics.RollupDay(ctx, f.pool, time.Now()); err != nil {
		t.Fatal(err)
	}
	checks := []struct{ sql, want string }{
		{"select visitors||'/'||sessions||'/'||pageviews||'/'||signups from daily_overview", "3/3/5/1"},
		{"select sum(views)||'/'||sum(exits) from daily_page_stats where route = '/'", "3/1"},
		{"select sum(exits)::text from daily_page_stats where route = '/pricing'", "1"},
		{"select (load_ms_sum / load_ms_count)::text from daily_page_stats where route = '/' and device = 'desktop'", "1200"},
		{"select string_agg(source||':'||sessions||':'||signups, ',' order by source) from daily_source_stats", "direct:1:0,google.com:1:1,newsletter:1:0"},
		{"select campaign from daily_source_stats where source = 'newsletter'", "nov"},
		{"select label||':'||clicks from daily_click_stats", "Get started:1"},
		{"select name||':'||events from daily_event_stats", "video_played:1"},
		{"select string_agg(value||':'||sessions, ',' order by value) from daily_dimension_stats where dimension = 'device'", "desktop:1,mobile:2"},
		{"select g.name||':'||s.conversions from daily_goal_stats s join goals g on g.id = s.goal_id", "Sign-ups:1"},
		{"select string_agg(step||':'||total, ',' order by step) from (select step, sum(sessions) as total from daily_funnel_stats group by step) x", "0:3,1:1,2:1"},
	}
	for _, c := range checks {
		if got := f.val(c.sql); got != c.want {
			t.Errorf("%s\n got  %s\n want %s", c.sql, got, c.want)
		}
	}

	if err := analytics.RollupRetention(ctx, f.pool); err != nil {
		t.Fatal(err)
	}
	if s := f.val("select week_offset||':'||accounts from weekly_retention"); s != "0:1" {
		t.Errorf("retention = %s", s)
	}
}

func TestConsentOffSendsNothing(t *testing.T) {
	// Server side: an empty or unknown batch stores nothing (the SDK sends no
	// analytics at all when window.__fixConsent === false).
	f := setup(t)
	req, _ := http.NewRequest("POST", f.srv.URL+"/v1/a?sentry_key="+f.p.PublicKey, strings.NewReader(`{"session_id":"s","events":[]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 204 {
		t.Fatalf("%v %v", err, resp)
	}
	if n := f.val("select count(*)::text from analytics_events"); n != "0" {
		t.Errorf("stored %s events", n)
	}
}

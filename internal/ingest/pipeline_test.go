package ingest_test

// End-to-end ingest against a real Postgres. Runs only when
// FIX_TEST_DATABASE_URL is set; it DROPS AND RECREATES the public schema of
// that database, so never point it at real data.
//
//	FIX_TEST_DATABASE_URL=postgres://fix@127.0.0.1:5433/fix_test go test ./internal/ingest -run Pipeline -v

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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/fixassist/internal/ingest"
	"github.com/yourname/fixassist/internal/jobs"
	"github.com/yourname/fixassist/internal/projects"
	"github.com/yourname/fixassist/internal/store"
	"github.com/yourname/fixassist/internal/store/db"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
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
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.Migrate(ctx, pool, log); err != nil {
		t.Fatal(err)
	}
	if err := jobs.RunPartitions(ctx, pool, log, time.Now()); err != nil {
		t.Fatal(err)
	}
	return pool
}

func goldenEnvelopes(t *testing.T) [][]byte {
	files, _ := filepath.Glob("testdata/envelopes/*.envelope")
	sort.Strings(files)
	if len(files) == 0 {
		t.Skip("no golden envelopes")
	}
	var out [][]byte
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

type env struct {
	pool    *pgxpool.Pool
	svc     *projects.Service
	cache   *projects.Cache
	limits  *projects.Limits
	srv     *httptest.Server
	project projects.Project
}

func setup(t *testing.T, samples *ingest.SampleCap) *env {
	ctx := context.Background()
	pool := testPool(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := &projects.Service{Pool: pool}
	p, err := svc.Create(ctx, projects.CreateParams{Org: "acme", Name: "spike", Agent: "lovable", Backend: "supabase", AllowedDomains: []string{"localhost"}})
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
	limits := projects.NewLimits()
	cache := &projects.Cache{Q: db.New(pool), Limits: limits, Log: log}
	if err := cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&ingest.Handler{Sink: &ingest.PGSink{Pool: pool, River: rc}, Log: log, Projects: cache, Limits: limits, Samples: samples}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &env{pool: pool, svc: svc, cache: cache, limits: limits, srv: srv, project: p}
}

func (e *env) post(t *testing.T, path, key, origin string, body []byte) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", e.srv.URL+path+"?sentry_key="+key, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func TestPipelineGoldenEnvelopes(t *testing.T) {
	e := setup(t, &ingest.SampleCap{})
	envs := goldenEnvelopes(t)
	path := fmt.Sprintf("/api/%d/envelope/", e.project.ID)
	for i, b := range envs {
		if resp := e.post(t, path, e.project.PublicKey, "http://localhost:4173", b); resp.StatusCode != 200 {
			t.Fatalf("envelope %d: status %d", i, resp.StatusCode)
		}
	}
	n := int64(len(envs))
	if got := count(t, e.pool, "select count(*) from events where project_id = $1", e.project.ID); got != n {
		t.Fatalf("events = %d, want %d", got, n)
	}
	if got := count(t, e.pool, "select count(*) from river_job where kind = 'group_event'"); got != n {
		t.Errorf("group_event jobs = %d, want %d", got, n)
	}
	if got := count(t, e.pool, "select events from project_usage_monthly where project_id = $1", e.project.ID); got != n {
		t.Errorf("usage = %d, want %d", got, n)
	}
	if got := count(t, e.pool, "select count(*) from events where kind = 'net'"); got != 10 {
		t.Errorf("net events = %d, want 10", got)
	}
	if got := count(t, e.pool, "select count(distinct fingerprint) from events"); got != 8 {
		t.Errorf("distinct fingerprints = %d, want 8 (one per case, stable across builds)", got)
	}
	if got := count(t, e.pool, "select count(*) from events where user_hash is null or length(user_hash) <> 32"); got != 0 {
		t.Errorf("%d events without a user hash", got)
	}
	if got := count(t, e.pool, "select count(*) from events where release_id like 'fix-%' and route = '/'"); got != n {
		t.Errorf("release/route missing on %d events", n-got)
	}
	for _, bad := range []string{"@example.com", "eyJhbGci", "session_id", `"id":`} {
		if got := count(t, e.pool, "select count(*) from events where payload::text like '%' || $1 || '%'", bad); got != 0 {
			t.Errorf("%d stored payloads contain %q", got, bad)
		}
	}

	// The stub worker marks every event grouped.
	deadline := time.Now().Add(15 * time.Second)
	for {
		left := count(t, e.pool, "select count(*) from events where grouped_at is null")
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d events still ungrouped", left)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestPipelineRejections(t *testing.T) {
	e := setup(t, &ingest.SampleCap{})
	body := goldenEnvelopes(t)[0]
	path := fmt.Sprintf("/api/%d/envelope/", e.project.ID)
	cases := []struct {
		name, path, key, origin string
		want                    int
	}{
		{"unknown key", path, "nope", "http://localhost:4173", 401},
		{"wrong project", "/api/999/envelope/", e.project.PublicKey, "http://localhost:4173", 403},
		{"foreign origin", path, e.project.PublicKey, "https://copied-snippet.example", 403},
		{"ok", path, e.project.PublicKey, "http://localhost:4173", 200},
	}
	for _, c := range cases {
		if resp := e.post(t, c.path, c.key, c.origin, body); resp.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}

	// Rotation: the old key stops working once the cache refreshes.
	ctx := context.Background()
	newKey, err := e.svc.RotateKey(ctx, e.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if resp := e.post(t, path, e.project.PublicKey, "http://localhost:4173", body); resp.StatusCode != 401 {
		t.Errorf("revoked key: status %d", resp.StatusCode)
	}
	if resp := e.post(t, path, newKey, "http://localhost:4173", body); resp.StatusCode != 200 {
		t.Errorf("new key: status %d", resp.StatusCode)
	}

	// Monthly quota: 2 events used, quota 3 -> one more, then 429.
	if err := e.svc.SetQuota(ctx, e.project.ID, 3); err != nil {
		t.Fatal(err)
	}
	if err := e.cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if resp := e.post(t, path, newKey, "http://localhost:4173", body); resp.StatusCode != 200 {
		t.Errorf("3rd event: status %d", resp.StatusCode)
	}
	resp := e.post(t, path, newKey, "http://localhost:4173", body)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" || !strings.HasSuffix(resp.Header.Get("X-Sentry-Rate-Limits"), "::key") {
		t.Errorf("over quota: status %d headers %v", resp.StatusCode, resp.Header)
	}
	if got := count(t, e.pool, "select count(*) from events"); got != 3 {
		t.Errorf("events stored = %d, want 3", got)
	}
}

func TestPipelineSampleCapAndPageviews(t *testing.T) {
	e := setup(t, &ingest.SampleCap{Limit: 1})
	body := goldenEnvelopes(t)[0]
	path := fmt.Sprintf("/api/%d/envelope/", e.project.ID)
	for i := 0; i < 3; i++ {
		if resp := e.post(t, path, e.project.PublicKey, "http://localhost:4173", body); resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	if got := count(t, e.pool, "select count(*) from events where payload is not null"); got != 1 {
		t.Errorf("payloads stored = %d, want 1", got)
	}
	if got := count(t, e.pool, "select count(*) from events"); got != 3 {
		t.Errorf("rows = %d, want 3 (counters keep counting over the cap)", got)
	}

	pv := []byte(`[{"path":"/orders/12345","session_id":"s1","release":"fix-1","load_ms":321},{"path":"/"}]`)
	if resp := e.post(t, "/v1/pv", e.project.PublicKey, "http://localhost:4173", pv); resp.StatusCode != 204 {
		t.Fatalf("pageview status %d", resp.StatusCode)
	}
	if got := count(t, e.pool, "select count(*) from pageviews where route = '/orders/<id>' and load_ms = 321"); got != 1 {
		t.Errorf("templated pageview rows = %d", got)
	}
}

func TestPartitionsRetention(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	old := time.Now().AddDate(0, 0, -40)
	if _, err := store.EnsurePartitions(ctx, pool, old, 0, 0); err != nil {
		t.Fatal(err)
	}
	dropped, err := store.DropOldPartitions(ctx, pool, time.Now(), store.RetentionDays)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != len(store.PartitionedTables) {
		t.Errorf("dropped = %v, want one partition per table from 40 days ago", dropped)
	}
	// Today's partitions survive and a rerun is a no-op.
	if again, _ := store.DropOldPartitions(ctx, pool, time.Now(), store.RetentionDays); len(again) != 0 {
		t.Errorf("rerun dropped %v", again)
	}
	if _, err := store.EnsurePartitions(ctx, pool, time.Now(), 1, 7); err != nil {
		t.Errorf("ensure rerun: %v", err)
	}
}

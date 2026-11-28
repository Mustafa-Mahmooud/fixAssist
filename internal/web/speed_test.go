package web_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yourname/fixassist/internal/auth"
	"github.com/yourname/fixassist/internal/jobs"
	"github.com/yourname/fixassist/internal/projects"
	"github.com/yourname/fixassist/internal/web"
)

// The Speed screen: nothing yet, then each page's p75 with its rating and
// what's behind the slow visits, for all devices or phones only.
func TestSpeedPage(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	as := &auth.Service{Pool: f.pool, Sender: f.mail, BaseURL: srv.URL}
	(&web.App{Pool: f.pool, Auth: as, Projects: &projects.Service{Pool: f.pool}, IngestBase: srv.URL, BaseURL: srv.URL,
		Secret: []byte("test-secret-test-secret-test-secret"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Register(mux)
	link, err := as.AdminLink(ctx, "founder@example.com")
	if err != nil {
		t.Fatal(err)
	}
	c := client(t)
	resp, _ := c.PostForm(srv.URL+"/auth/magic", url.Values{"token": {reToken.FindStringSubmatch(link)[1]}})
	body(t, resp)
	var orgID int64
	_ = f.pool.QueryRow(ctx, "select m.org_id from memberships m join users u on u.id = m.user_id where u.email = 'founder@example.com'").Scan(&orgID)
	p, err := (&projects.Service{Pool: f.pool}).Create(ctx, projects.CreateParams{OrgID: orgID, Name: "shop", Agent: "cursor", Backend: "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.RunPartitions(ctx, f.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now()); err != nil {
		t.Fatal(err)
	}
	speed := fmt.Sprintf("%s/projects/%d/speed", srv.URL, p.ID)

	resp, _ = c.Get(speed)
	if b := body(t, resp); resp.StatusCode != 200 || !strings.Contains(b, "No speed measurements yet") || !strings.Contains(b, `href="/projects/`+fmt.Sprint(p.ID)+`/speed"`) {
		t.Fatalf("empty speed page: %d", resp.StatusCode)
	}

	// /pricing on computers: loads mostly in [1400, 1600) ms (bucket 16),
	// taps slow (INP in [400, 500), bucket 8) because of button#buy.
	if _, err := f.pool.Exec(ctx, `insert into daily_vitals (project_id, day, route, device, release_id, metric, bucket, n) values
		($1, current_date, '/pricing', 'desktop', 'r1', 'LCP', 16, 30),
		($1, current_date, '/pricing', 'desktop', 'r1', 'INP', 8, 25),
		($1, current_date, '/pricing', 'desktop', 'r1', 'CLS', 2, 30),
		($1, current_date, '/', 'mobile', 'r1', 'LCP', 16, 3)`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `insert into analytics_events (project_id, ts, type, route, name, props, device)
		select $1, now(), 'vital', '/pricing', 'INP', '{"v": 450, "el": "button#buy", "src": "https://app.example.com/assets/index.js"}', 'desktop'
		from generate_series(1, 5)`, p.ID); err != nil {
		t.Fatal(err)
	}
	resp, _ = c.Get(speed)
	b := body(t, resp)
	for _, want := range []string{"<code>/pricing</code>", "1.6 s", "Needs work", "475 ms", "<code>button#buy</code>", "index.js", "100% of slow visits", "Fewer than 20 measurements"} {
		if !strings.Contains(b, want) {
			t.Errorf("speed page doesn't contain %q", want)
		}
	}
	if strings.Index(b, "<code>/pricing</code>") > strings.Index(b, "<code>/</code>") {
		t.Error("the page with problems isn't listed first")
	}
	resp, _ = c.Get(speed + "?device=mobile")
	if b := body(t, resp); strings.Contains(b, "<code>/pricing</code>") || !strings.Contains(b, "<code>/</code>") {
		t.Error("phones filter shows computer measurements")
	}
}

// A slowdown's issue page shows before and after, not error counts.
func TestSlowdownIssuePage(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	as := &auth.Service{Pool: f.pool, Sender: f.mail, BaseURL: srv.URL}
	(&web.App{Pool: f.pool, Auth: as, Projects: &projects.Service{Pool: f.pool}, IngestBase: srv.URL, BaseURL: srv.URL,
		Secret: []byte("test-secret-test-secret-test-secret"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Register(mux)
	link, err := as.AdminLink(ctx, "founder@example.com")
	if err != nil {
		t.Fatal(err)
	}
	c := client(t)
	resp, _ := c.PostForm(srv.URL+"/auth/magic", url.Values{"token": {reToken.FindStringSubmatch(link)[1]}})
	body(t, resp)
	var orgID int64
	_ = f.pool.QueryRow(ctx, "select m.org_id from memberships m join users u on u.id = m.user_id where u.email = 'founder@example.com'").Scan(&orgID)
	p, err := (&projects.Service{Pool: f.pool}).Create(ctx, projects.CreateParams{OrgID: orgID, Name: "shop", Agent: "cursor", Backend: "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := f.pool.QueryRow(ctx, `insert into issues (project_id, fingerprint, kind, signature, title, route, status, first_seen_at, last_seen_at, event_count)
		values ($1, 'perf:LCP:/pricing', 'perf', 'perf:LCP:/pricing', 'Loading got slower on /pricing', '/pricing', 'new', now(), now(), 80) returning id`, p.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `insert into perf_regressions (issue_id, project_id, route, metric, device, baseline_p75, baseline_n, current_p75, current_n, current_release, since, element, source, breakdown)
		values ($1, $2, '/pricing', 'LCP', 'mobile', 1500, 150, 5400, 80, 'r2', now() - interval '1 day', 'main>img.hero', 'https://cdn.example.com/hero.png?', '{"rlt": 4200, "ttfb": 300}')`, id, p.ID); err != nil {
		t.Fatal(err)
	}
	resp, _ = c.Get(fmt.Sprintf("%s/projects/%d/issues/%d", srv.URL, p.ID, id))
	b := body(t, resp)
	for _, want := range []string{"Slow page", "Loading now", "5.4 s", "Poor", "1.5 s", "Phones", "Where the time goes", "Download 4.2 s", "<code>main&gt;img.hero</code>", "80</b>"} {
		if !strings.Contains(b, want) {
			t.Errorf("slowdown page doesn't contain %q", want)
		}
	}
	for _, not := range []string{"People affected", "How often it happened"} {
		if strings.Contains(b, not) {
			t.Errorf("slowdown page shows %q", not)
		}
	}
}

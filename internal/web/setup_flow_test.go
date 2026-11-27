package web_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The easy-setup path: address + builder only, refused sites with "Allow",
// the first issue celebrated, and "Check my site" (offline cases only).
func TestEasySetup(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	c, csrf := f.signIn(t, "easy@example.com")
	get := func(path string) (string, *http.Response) {
		t.Helper()
		req, _ := http.NewRequest("GET", f.srv.URL+path, nil)
		req.Header.Set("HX-Request", "true")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return body(t, resp), resp
	}
	post := func(path string, v url.Values) (string, *http.Response) {
		t.Helper()
		v.Set("csrf", csrf)
		resp, err := c.PostForm(f.srv.URL+path, v)
		if err != nil {
			t.Fatal(err)
		}
		return body(t, resp), resp
	}
	has := func(what, b string, wants ...string) {
		t.Helper()
		for _, w := range wants {
			if !strings.Contains(b, w) {
				t.Errorf("%s missing %q", what, w)
			}
		}
	}

	b, _ := get("/projects")
	has("welcome form", b, `name="address"`, `type="radio" name="agent" value="lovable" checked`, "Something else", "Give it a different name")
	if strings.Contains(b, `name="backend"`) {
		t.Error("the form still asks for the backend")
	}

	// The editor's address is refused with a specific hint, keeping the input.
	b, resp := post("/projects", url.Values{"address": {"https://lovable.dev/projects/abc"}, "agent": {"bolt"}})
	if resp.StatusCode != 400 {
		t.Errorf("editor URL: status %d", resp.StatusCode)
	}
	has("editor URL error", b, "Lovable&#39;s editor", `value="https://lovable.dev/projects/abc"`, `value="bolt" checked`)

	// Address and builder only: name from the address, backend left to detection.
	_, resp = post("/projects", url.Values{"address": {"https://MyShop.lovable.app/pricing"}, "agent": {"bolt"}})
	if !strings.HasSuffix(resp.Request.URL.Path, "/setup") {
		t.Fatalf("after create: %s", resp.Request.URL)
	}
	base := strings.TrimSuffix(resp.Request.URL.Path, "/setup")
	var pid int64
	var name, backend, agent string
	var domains []string
	if err := f.pool.QueryRow(ctx, "select id, name, backend, target_agent, allowed_domains from projects where name = 'myshop'").Scan(&pid, &name, &backend, &agent, &domains); err != nil {
		t.Fatal(err)
	}
	if backend != "unknown" || agent != "bolt" || len(domains) != 1 || domains[0] != "myshop.lovable.app" {
		t.Errorf("project = %s %s %s %v", name, backend, agent, domains)
	}

	// A refused preview address is offered with "Allow", then allowed.
	if _, err := f.pool.Exec(ctx, "insert into project_rejected_origins (project_id, host, hits) values ($1, 'preview--myshop.lovable.app', 4)", pid); err != nil {
		t.Fatal(err)
	}
	b, _ = get(base + "/setup/status")
	has("status with a refused site", b, "Waiting for the first visit", "were refused", "<code>preview--myshop.lovable.app</code>", "4 times", `action="`+base+`/domains"`, `hx-trigger="every 3s"`)
	b, resp = post(base+"/domains", url.Values{"host": {"preview--myshop.lovable.app"}})
	if resp.StatusCode != 200 || resp.Request.URL.Query().Get("allowed") != "preview--myshop.lovable.app" {
		t.Fatalf("allow: %d %s", resp.StatusCode, resp.Request.URL)
	}
	has("setup after allowing", b, "Allowed <code>preview--myshop.lovable.app</code>", "<code>myshop.lovable.app</code>, <code>preview--myshop.lovable.app</code>")
	if b, _ = get(base + "/setup/status"); strings.Contains(b, "were refused") {
		t.Error("allowed site still offered")
	}
	if _, resp = post(base+"/domains", url.Values{"host": {"lovable.dev"}}); resp.StatusCode != 400 {
		t.Errorf("allowing the editor: status %d", resp.StatusCode)
	}

	// The first issue: celebrated, linked, and polling stops.
	if err := ensurePartition(f.pool); err != nil {
		t.Fatal(err)
	}
	var issueID int64
	if err := f.pool.QueryRow(ctx, `insert into issues (project_id, fingerprint, kind, signature, title, route, first_seen_at, last_seen_at, event_count, user_count, impact_score)
		values ($1, 't', 'js', 's', 'Error: Fix Assistant test', '/', now(), now(), 1, 1, 1) returning id`, pid).Scan(&issueID); err != nil {
		t.Fatal(err)
	}
	b, _ = get(base + "/setup/status")
	has("status with an issue", b, "Your first issue is in", base+"/issues/")
	if strings.Contains(b, "hx-trigger") {
		t.Error("still polling after the first issue")
	}

	// One-click agent connections: a named token and the editor's deeplink.
	b, _ = post(base+"/agent-tokens", url.Values{"for": {"cursor"}})
	has("connect Cursor", b, "Add Fix Assistant to Cursor", `href="cursor://anysphere.cursor-deeplink/mcp/install?name=fix-assistant&amp;config=`)
	b, _ = post(base+"/agent-tokens", url.Values{"for": {"claude"}})
	has("connect Claude Code", b, "Paste this into your terminal", "claude mcp add --transport http fix-assistant")
	var names []string
	rows, _ := f.pool.Query(ctx, "select name from agent_tokens where project_id = $1 order by id", pid)
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	if strings.Join(names, ",") != "Cursor,Claude Code" {
		t.Errorf("token names = %v", names)
	}

	// Auto-fix pairing: one command with a fresh one-time code.
	b, _ = post(base+"/autofix/pair", url.Values{})
	has("pair command", b, "/install/fixassist.ps1?pair=", "/install/fixassist.sh?pair=", "Or set it up by hand")
	if n := scalarInt(t, f, "select count(*) from pair_codes where project_id = $1 and used_at is null", pid); n != 1 {
		t.Errorf("pair codes = %d", n)
	}

	// Saving a setting inside a Setup tab comes back to that tab.
	noRedirect := *c
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for path, frag := range map[string]string{"/training": "#training", "/autofix": "#autofix"} {
		resp, err := noRedirect.PostForm(f.srv.URL+base+path, url.Values{"csrf": {csrf}, "rule": {"off"}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if loc := resp.Header.Get("Location"); !strings.HasSuffix(loc, "/setup"+frag) {
			t.Errorf("POST %s redirects to %q, want its tab %s", path, loc, frag)
		}
	}

	// Check my site, for an address that can't be fetched from here.
	if _, err := f.pool.Exec(ctx, "update projects set allowed_domains = '{localhost}' where id = $1", pid); err != nil {
		t.Fatal(err)
	}
	b, _ = post(base+"/setup/check", url.Values{})
	has("site check", b, `id="site-check"`, "your own computer")
	b, _ = post(base+"/setup/check", url.Values{})
	has("second site check", b, "Just a moment")
}

func scalarInt(t *testing.T, f *fixture, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

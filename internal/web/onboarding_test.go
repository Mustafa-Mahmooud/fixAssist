package web_test

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

// A first-time founder: welcome page, then a checklist that ticks itself
// from the data, page tips, and both can be put away.
func TestOnboarding(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	c, csrf := f.signIn(t, "new@example.com")
	must := func(b string, wants ...string) {
		t.Helper()
		for _, w := range wants {
			if !strings.Contains(b, w) {
				t.Errorf("page missing %q", w)
			}
		}
	}
	get := func(path string) string {
		t.Helper()
		resp, err := c.Get(f.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		return body(t, resp)
	}

	// No app yet: the welcome page explains the loop and has the form.
	must(get("/projects"), "Welcome to Fix Assistant", "How it works", "It checks the fix", "Add your first app", `action="/projects"`)

	resp, _ := c.PostForm(f.srv.URL+"/projects", url.Values{"csrf": {csrf}, "name": {"Shop"}, "agent": {"lovable"}, "backend": {"supabase"}})
	b := body(t, resp)
	setupPath := resp.Request.URL.Path
	base := strings.TrimSuffix(setupPath, "/setup")
	must(b, "Getting started", "1 of 4 done", "Install the snippet", "paste it into Lovable", "What this page shows:", "the snippet for your website")
	if strings.Contains(b, "You're set up") {
		t.Error("set up before anything was installed")
	}

	var pid int64
	_ = f.pool.QueryRow(ctx, "select id from projects where name = 'Shop'").Scan(&pid)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := ensurePartition(f.pool); err != nil {
		t.Fatal(err)
	}
	// The first visit ticks "Install the snippet".
	exec("insert into pageviews (project_id, route, session_id) values ($1, '/', 's1')", pid)
	must(get(base+"/overview"), "2 of 4 done", "Fix Assistant test", "What this page shows:")
	// The first issue, then its explanation.
	var issueID int64
	if err := f.pool.QueryRow(ctx, `insert into issues (project_id, fingerprint, kind, signature, title, route, first_seen_at, last_seen_at)
		values ($1, 'fp', 'js', 's', 'TypeError', '/', now(), now()) returning id`, pid).Scan(&issueID); err != nil {
		t.Fatal(err)
	}
	b = get(base + "/issues")
	must(b, "3 of 4 done", "Open the issue")
	exec(`insert into fix_suggestions (issue_id, origin, prompt_version, target_agent, severity, title, what_broke, who_is_affected, fix_prompt, how_to_check)
		values ($1, 'agent', 'v1', 'lovable', 'medium', 't', 'w', 'w', 'p', 'c')`, issueID)
	b = get(base + "/issues")
	must(b, "You're set up", "Catch errors from your server too", "Let your AI agent fix issues by itself")

	// Dismissed: gone from every page of this project.
	resp, _ = c.PostForm(f.srv.URL+base+"/onboarding/dismiss", url.Values{"csrf": {csrf}})
	body(t, resp)
	if b := get(base + "/overview"); strings.Contains(b, "You're set up") || strings.Contains(b, "Getting started") {
		t.Error("checklist shown after dismissing")
	}

	// Tips: hidden for this person, shown again from the projects page.
	resp, _ = c.PostForm(f.srv.URL+"/settings/tips", url.Values{"csrf": {csrf}})
	body(t, resp)
	if b := get(base + "/pages"); strings.Contains(b, "What this page shows:") {
		t.Error("tip shown after hiding tips")
	}
	b = get("/projects")
	must(b, "Page tips are hidden", "Shop")
	if strings.Contains(b, "Welcome to Fix Assistant") {
		t.Error("welcome page shown to someone who has an app")
	}
	resp, _ = c.PostForm(f.srv.URL+"/settings/tips", url.Values{"csrf": {csrf}, "show": {"1"}})
	body(t, resp)
	must(get(base+"/pages"), "What this page shows:")

	// Without the CSRF token, nothing changes.
	resp, _ = c.PostForm(f.srv.URL+"/settings/tips", url.Values{})
	if body(t, resp); resp.StatusCode != 403 {
		t.Errorf("tips without csrf: %d", resp.StatusCode)
	}
}

// The privacy policy is public, linked everywhere, and names the contact.
func TestPrivacyPage(t *testing.T) {
	f := setup(t)
	resp, err := client(t).Get(f.srv.URL + "/privacy")
	if err != nil {
		t.Fatal(err)
	}
	b := body(t, resp)
	if resp.StatusCode != 200 || resp.Request.URL.Path != "/privacy" {
		t.Fatalf("privacy page: %d %s (it must not need a sign-in)", resp.StatusCode, resp.Request.URL)
	}
	for _, want := range []string{"Privacy policy", "never collected", "IP addresses of page views are never stored", "30 days", "Northflank", "#for-customers", "/privacy</pre>"} {
		if !strings.Contains(b, want) {
			t.Errorf("privacy page missing %q", want)
		}
	}
	resp, _ = client(t).Get(f.srv.URL + "/login")
	if b := body(t, resp); !strings.Contains(b, `href="/privacy"`) {
		t.Error("no privacy link on the login page")
	}
}

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
	"github.com/yourname/fixassist/internal/projects"
	"github.com/yourname/fixassist/internal/web"
)

// Auto-fix on the dashboard: the Setup switch, the issue page, the signed
// email link (GET never approves), and staff can't start code on a
// founder's computer.
func TestAutofixDashboard(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	secret := []byte("test-secret-test-secret-test-secret")
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	as := &auth.Service{Pool: f.pool, Sender: f.mail, BaseURL: srv.URL}
	(&web.App{Pool: f.pool, Auth: as, Projects: &projects.Service{Pool: f.pool}, IngestBase: srv.URL, BaseURL: srv.URL,
		Secret: secret, Staff: []string{"ops@example.com"}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Register(mux)
	signIn := func(email string) (*http.Client, string) {
		link, err := as.AdminLink(ctx, email)
		if err != nil {
			t.Fatal(err)
		}
		c := client(t)
		resp, _ := c.PostForm(srv.URL+"/auth/magic", url.Values{"token": {reToken.FindStringSubmatch(link)[1]}})
		m := reCSRF.FindStringSubmatch(body(t, resp))
		if m == nil {
			t.Fatal("no csrf")
		}
		return c, m[1]
	}
	founder, csrf := signIn("founder@example.com")
	var orgID int64
	_ = f.pool.QueryRow(ctx, "select m.org_id from memberships m join users u on u.id = m.user_id where u.email = 'founder@example.com'").Scan(&orgID)
	p, err := (&projects.Service{Pool: f.pool}).Create(ctx, projects.CreateParams{OrgID: orgID, Name: "shop", Agent: "cursor", Backend: "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("%s/projects/%d", srv.URL, p.ID)

	// No daemon yet: the switch can't be turned on.
	resp, _ := founder.Get(base + "/setup")
	b := body(t, resp)
	for _, want := range []string{`id="autofix"`, "No computer connected yet", "Get my install command", "fixassist add --server " + srv.URL, "irm " + srv.URL + "/install/fixassist.ps1 | iex", "curl -fsSL " + srv.URL + "/install/fixassist.sh | sh", `href="/download/fixassist/fixassist-windows-amd64.exe"`, `name="enabled"  disabled`} {
		if !strings.Contains(b, want) {
			t.Errorf("setup missing %q", want)
		}
	}

	var issueID, sid, tokenID int64
	_ = f.pool.QueryRow(ctx, `insert into issues (project_id, fingerprint, kind, signature, title, route, status, first_seen_at, last_seen_at, event_count)
		values ($1, 'fp', 'net', 'sig', 'Checkout fails', '/checkout', 'explained', now(), now(), 3) returning id`, p.ID).Scan(&issueID)
	_ = f.pool.QueryRow(ctx, `insert into fix_suggestions (issue_id, origin, prompt_version, target_agent, severity, title, what_broke, who_is_affected, fix_prompt, how_to_check)
		values ($1, 'catalog', 'v1', 'cursor', 'high', 'Checkout fails', 'w', 'w', 'p', 'c') returning id`, issueID).Scan(&sid)
	_ = f.pool.QueryRow(ctx, `insert into agent_tokens (project_id, name, token_hash, prefix) values ($1, 'laptop', 'h', 'fa_x') returning id`, p.ID).Scan(&tokenID)
	if _, err := f.pool.Exec(ctx, `insert into agent_daemons (token_id, project_id, hostname) values ($1, $2, 'my-laptop')`, tokenID, p.ID); err != nil {
		t.Fatal(err)
	}

	issueURL := fmt.Sprintf("%s/issues/%d", base, issueID)
	resp, _ = founder.Get(issueURL)
	if b := body(t, resp); !strings.Contains(b, "Set up auto-fix") {
		t.Error("issue page doesn't offer setting up auto-fix")
	}
	// Starting it while off is refused.
	resp, _ = founder.PostForm(issueURL+"/autofix", url.Values{"csrf": {csrf}})
	if body(t, resp); resp.StatusCode != http.StatusConflict {
		t.Errorf("start while off: %d", resp.StatusCode)
	}

	resp, _ = founder.PostForm(base+"/autofix", url.Values{"csrf": {csrf}, "enabled": {"on"}, "rule": {"off"}})
	if b := body(t, resp); !strings.Contains(b, "my-laptop") || !strings.Contains(b, `name="enabled" checked`) {
		t.Errorf("setup after enabling: %d", resp.StatusCode)
	}

	// A waiting run, approved from the email link.
	var runID int64
	_ = f.pool.QueryRow(ctx, `insert into autofix_runs (project_id, issue_id, suggestion_id, state) values ($1, $2, $3, 'pending_approval') returning id`, p.ID, issueID, sid).Scan(&runID)
	link := web.AutofixLink(srv.URL, secret, runID, time.Now())
	anon := client(t)
	resp, _ = anon.Get(link)
	if b := body(t, resp); resp.StatusCode != 200 || !strings.Contains(b, "Yes, auto-fix it") {
		t.Fatalf("confirm page: %d %s", resp.StatusCode, b)
	}
	state := func() string {
		var s string
		_ = f.pool.QueryRow(ctx, "select state from autofix_runs where id = $1", runID).Scan(&s)
		return s
	}
	if state() != "pending_approval" {
		t.Fatal("GET approved the run")
	}
	for _, bad := range []string{strings.Replace(link, fmt.Sprintf("/a/%d?", runID), fmt.Sprintf("/a/%d?", runID+1), 1),
		web.AutofixLink(srv.URL, secret, runID, time.Now().Add(-8*24*time.Hour))} {
		resp, _ := anon.PostForm(strings.Split(bad, "?")[0], parseQuery(t, bad))
		if body(t, resp); resp.StatusCode != 400 {
			t.Errorf("bad link accepted: %d", resp.StatusCode)
		}
	}
	resp, _ = anon.PostForm(strings.Split(link, "?")[0], parseQuery(t, link))
	if b := body(t, resp); !strings.Contains(b, "Approved.") || state() != "approved" {
		t.Fatalf("submit: %s state=%s", b, state())
	}
	resp, _ = anon.PostForm(strings.Split(link, "?")[0], parseQuery(t, link))
	if b := body(t, resp); !strings.Contains(b, "already approved") {
		t.Errorf("second submit: %s", b)
	}

	// The issue page follows it, and can cancel it.
	resp, _ = founder.Get(issueURL)
	if b := body(t, resp); !strings.Contains(b, "Approved, waiting for the daemon") || !strings.Contains(b, fmt.Sprintf("/autofix/%d/reject", runID)) {
		t.Error("issue page doesn't show the approved run")
	}

	// Staff can't touch auto-fix on another account's app, even where they
	// otherwise have access (403 from autofixProject, or 404 when staff
	// access is read-only).
	staff, staffCSRF := signIn("ops@example.com")
	blocked := func(code int) bool { return code == http.StatusForbidden || code == http.StatusNotFound }
	resp, _ = staff.PostForm(fmt.Sprintf("%s/autofix/%d/reject", base, runID), url.Values{"csrf": {staffCSRF}})
	if body(t, resp); !blocked(resp.StatusCode) || state() != "approved" {
		t.Errorf("staff reject: %d state=%s", resp.StatusCode, state())
	}
	resp, _ = staff.PostForm(base+"/autofix", url.Values{"csrf": {staffCSRF}})
	if body(t, resp); !blocked(resp.StatusCode) {
		t.Errorf("staff switch: %d", resp.StatusCode)
	}

	resp, _ = founder.PostForm(fmt.Sprintf("%s/autofix/%d/reject", base, runID), url.Values{"csrf": {csrf}})
	if body(t, resp); state() != "rejected" {
		t.Errorf("reject: state=%s", state())
	}
	// Started again from the issue page.
	resp, _ = founder.PostForm(issueURL+"/autofix", url.Values{"csrf": {csrf}})
	if b := body(t, resp); !strings.Contains(b, "Approved, waiting for the daemon") {
		t.Errorf("start: %d", resp.StatusCode)
	}
	if b := body(t, resp); strings.Contains(b, "Your daemon is connected, but") {
		t.Error("nothing holds the run, but the page says something does")
	}

	// The daemon is polling but the server won't hand the run out: the page
	// says why instead of "waiting for the daemon".
	held := func(why, setup string) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, setup, p.ID); err != nil {
			t.Fatal(err)
		}
		resp, _ := founder.Get(issueURL)
		if b := body(t, resp); !strings.Contains(b, why) {
			t.Errorf("held run: page doesn't say %q", why)
		}
	}
	held("auto-fix is turned off", `update projects set autofix_enabled = false where id = $1`)
	if _, err := f.pool.Exec(ctx, `update projects set autofix_enabled = true where id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	// A run on another issue is in progress.
	held("working on another fix first", `with other as (
		insert into issues (project_id, fingerprint, kind, signature, title, route, status, first_seen_at, last_seen_at, event_count)
		values ($1, 'fp2', 'net', 'sig2', 'Login fails', '/login', 'explained', now(), now(), 1) returning id, project_id)
		insert into autofix_runs (project_id, issue_id, state, claimed_at) select project_id, id, 'claimed', now() from other`)
	if _, err := f.pool.Exec(ctx, `update autofix_runs set state = 'failed' where project_id = $1 and state = 'claimed'`, p.ID); err != nil {
		t.Fatal(err)
	}
	// No daily limit: many fixes today don't hold the next one.
	if _, err := f.pool.Exec(ctx, `insert into autofix_runs (project_id, issue_id, state, claimed_at)
		select project_id, issue_id, 'failed', now() from autofix_runs, generate_series(1, 9)
		where project_id = $1 and state = 'failed'`, p.ID); err != nil {
		t.Fatal(err)
	}
	resp, _ = founder.Get(issueURL)
	if b := body(t, resp); strings.Contains(b, "Your daemon is connected, but") {
		t.Error("ten fixes today hold the next one")
	}
}

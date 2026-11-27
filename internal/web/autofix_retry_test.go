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

	"github.com/yourname/fixassist/internal/auth"
	"github.com/yourname/fixassist/internal/projects"
	"github.com/yourname/fixassist/internal/web"
)

// A run that couldn't find the code: the issue page says why and where it
// looked, offers "Try again" with a note for the agent, and Setup takes the
// branch to fix from.
func TestAutofixNotInRepoPage(t *testing.T) {
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
	m := reCSRF.FindStringSubmatch(body(t, resp))
	if m == nil {
		t.Fatal("no csrf")
	}
	csrf := m[1]
	var orgID int64
	_ = f.pool.QueryRow(ctx, "select m.org_id from memberships m join users u on u.id = m.user_id where u.email = 'founder@example.com'").Scan(&orgID)
	p, err := (&projects.Service{Pool: f.pool}).Create(ctx, projects.CreateParams{OrgID: orgID, Name: "shop", Agent: "cursor", Backend: "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("%s/projects/%d", srv.URL, p.ID)
	var issueID int64
	_ = f.pool.QueryRow(ctx, `insert into issues (project_id, fingerprint, kind, signature, title, route, status, first_seen_at, last_seen_at, event_count)
		values ($1, 'fp', 'server', 'sig', 'ArgumentException: name', '/LearningErrors/name', 'explained', now(), now(), 3) returning id`, p.ID).Scan(&issueID)
	if _, err := f.pool.Exec(ctx, `insert into fix_suggestions (issue_id, origin, prompt_version, target_agent, severity, title, what_broke, who_is_affected, fix_prompt, how_to_check)
		values ($1, 'catalog', 'v1', 'cursor', 'high', 'Name', 'w', 'w', 'p', 'c')`, issueID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `update projects set autofix_enabled = true where id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `insert into autofix_runs (project_id, issue_id, state, error, fail_reason)
		values ($1, $2, 'failed', 'Searched for /LearningErrors/name. It looked in origin/master (commit 6174f0a).', 'not_in_repo')`, p.ID, issueID); err != nil {
		t.Fatal(err)
	}
	issueURL := fmt.Sprintf("%s/issues/%d", base, issueID)
	// The founder's computer still runs fixassist 0.2.1.
	var tokenID int64
	_ = f.pool.QueryRow(ctx, `insert into agent_tokens (project_id, name, token_hash, prefix) values ($1, 'laptop', 'h', 'fa_x') returning id`, p.ID).Scan(&tokenID)
	if _, err := f.pool.Exec(ctx, `insert into agent_daemons (token_id, project_id, hostname, version) values ($1, $2, 'my-laptop', '0.2.1')`, tokenID, p.ID); err != nil {
		t.Fatal(err)
	}

	resp, _ = c.Get(issueURL)
	b := body(t, resp)
	for _, want := range []string{"couldn't find the code behind this error", "origin/master (commit 6174f0a)", "the branch fixassist was set up with",
		`#autofix-branch">change the branch`, `<details open>`, "Tell the agent where to look", `name="hint"`, ">Try again</button>"} {
		if !strings.Contains(b, want) {
			t.Errorf("issue page doesn't contain %q", want)
		}
	}

	// Setup: a branch auto-fix can use, and one it can't.
	resp, _ = c.PostForm(base+"/autofix/branch", url.Values{"csrf": {csrf}, "branch": {"--upload-pack=x"}})
	if body(t, resp); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad branch: %d", resp.StatusCode)
	}
	resp, _ = c.PostForm(base+"/autofix/branch", url.Values{"csrf": {csrf}, "branch": {"learning"}})
	if b := body(t, resp); !strings.Contains(b, `value="learning"`) || !strings.Contains(b, "Your fixassist is v0.2.1</b>, which ignores this setting") {
		t.Error("setup doesn't show the saved branch, or doesn't warn about the old daemon")
	}
	resp, _ = c.Get(issueURL)
	if b := body(t, resp); !strings.Contains(b, "<code>learning</code>") || !strings.Contains(b, "Your fixassist is v0.2.1, which ignores that setting") {
		t.Error("issue page doesn't name the branch, or doesn't say the daemon is too old for it")
	}
	// Updated: no warning.
	if _, err := f.pool.Exec(ctx, `update agent_daemons set version = '0.3.0' where token_id = $1`, tokenID); err != nil {
		t.Fatal(err)
	}
	resp, _ = c.Get(base + "/setup")
	if b := body(t, resp); strings.Contains(b, "which ignores this setting") {
		t.Error("warns about an up-to-date daemon")
	}

	// Try again, with a note.
	resp, _ = c.PostForm(issueURL+"/autofix", url.Values{"csrf": {csrf}, "hint": {"  It's in Controllers/LearningErrors.cs  "}})
	b = body(t, resp)
	var hint, state string
	_ = f.pool.QueryRow(ctx, `select hint, state from autofix_runs where issue_id = $1 order by id desc limit 1`, issueID).Scan(&hint, &state)
	if state != "approved" || hint != "It's in Controllers/LearningErrors.cs" {
		t.Errorf("retry: state %q hint %q", state, hint)
	}
	if !strings.Contains(b, "Approved, waiting for the daemon") || strings.Contains(b, "couldn't find the code behind") {
		t.Error("the issue page doesn't follow the new run")
	}
}

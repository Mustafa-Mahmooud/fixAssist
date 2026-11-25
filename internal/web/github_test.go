package web_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/yourname/fixassist/internal/ai"
	"github.com/yourname/fixassist/internal/auth"
	"github.com/yourname/fixassist/internal/github"
	"github.com/yourname/fixassist/internal/projects"
	"github.com/yourname/fixassist/internal/web"
)

func repoTarball(t *testing.T) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	src := "export async function saveProfile(email) {\n  return supabase.from('profiles').insert({ email })\n}\n"
	_ = tw.WriteHeader(&tar.Header{Name: "acme-app-1/src/pages/Signup.tsx", Mode: 0o644, Size: int64(len(src)), Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte(src))
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// fakeGitHub: user "good-code" can access installation 7 only; installation
// 7 has one repo, acme/app.
func fakeGitHub(t *testing.T) *httptest.Server {
	tarball := repoTarball(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
			w.WriteHeader(401)
			return
		}
		fmt.Fprintf(w, `{"token":"inst-%s","expires_at":"2099-01-01T00:00:00Z"}`, r.PathValue("id"))
	})
	mux.HandleFunc("GET /installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"repositories":[{"full_name":"acme/app"}]}`)
	})
	mux.HandleFunc("GET /repos/acme/app/tarball", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer inst-7" {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write(tarball)
	})
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		fmt.Fprintf(w, `{"access_token":"user-%s"}`, r.PostFormValue("code"))
	})
	mux.HandleFunc("GET /user/installations", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer user-good-code" {
			fmt.Fprint(w, `{"installations":[{"id":7}]}`)
			return
		}
		fmt.Fprint(w, `{"installations":[]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestGitHubConnectAndCodeContext(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	gh := fakeGitHub(t)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	ghApp := &github.App{ID: 1, Slug: "fix-assistant-test", ClientID: "cid", ClientSecret: "cs", Key: key, APIBase: gh.URL, WebBase: gh.URL}
	searcher := &github.Searcher{App: ghApp, Pool: f.pool}

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	as := &auth.Service{Pool: f.pool, Sender: f.mail, BaseURL: srv.URL}
	(&web.App{Pool: f.pool, Auth: as, Projects: &projects.Service{Pool: f.pool}, IngestBase: srv.URL, BaseURL: srv.URL,
		Secret: []byte("github-test-secret-github-test-secret"), GitHub: ghApp, Code: searcher,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Register(mux)

	c, csrf := f.signIn(t, "gh@example.com")
	var orgID int64
	_ = f.pool.QueryRow(ctx, "select m.org_id from memberships m join users u on u.id = m.user_id where u.email = 'gh@example.com'").Scan(&orgID)
	p, err := (&projects.Service{Pool: f.pool}).Create(ctx, projects.CreateParams{OrgID: orgID, Name: "app", Agent: "cursor", Backend: "supabase"})
	if err != nil {
		t.Fatal(err)
	}
	// The session cookie was issued for f.srv; reuse it against this server.
	u, _ := url.Parse(f.srv.URL)
	u2, _ := url.Parse(srv.URL)
	c.Jar.SetCookies(u2, c.Jar.Cookies(u))

	// Connect: we're sent to GitHub's install page with a signed state.
	noRedirect := *c
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirect.PostForm(fmt.Sprintf("%s/projects/%d/github/connect", srv.URL, p.ID), url.Values{"csrf": {csrf}})
	if err != nil || resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("connect: %v %v", err, resp)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if !strings.HasPrefix(loc.String(), gh.URL+"/apps/fix-assistant-test/installations/new") {
		t.Fatalf("install URL = %s", loc)
	}
	state := loc.Query().Get("state")

	// Someone else's installation (8) is refused.
	resp, _ = c.Get(fmt.Sprintf("%s/github/setup?installation_id=8&code=good-code&state=%s", srv.URL, url.QueryEscape(state)))
	if body(t, resp); resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign installation: %d, want 403", resp.StatusCode)
	}
	// A tampered state is refused.
	resp, _ = c.Get(fmt.Sprintf("%s/github/setup?installation_id=7&code=good-code&state=%s", srv.URL, url.QueryEscape(state+"x")))
	if body(t, resp); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("tampered state: %d, want 400", resp.StatusCode)
	}
	// The real flow: installation 7, one repo → picked automatically.
	resp, _ = c.Get(fmt.Sprintf("%s/github/setup?installation_id=7&code=good-code&state=%s", srv.URL, url.QueryEscape(state)))
	b := body(t, resp)
	if !strings.Contains(b, "Connected to <code>acme/app</code>") {
		t.Fatalf("setup page after connect (%d):\n%s", resp.StatusCode, b)
	}

	// An issue on profiles (22001) now gets a file-level prompt.
	var issueID int64
	payload := `{"exception":{"values":[{"type":"HTTPError","value":"POST /rest/v1/profiles 400"}]},"request":{"url":"https://app.example.com/signup"},
	  "contexts":{"fix_http":{"method":"POST","url":"https://x.supabase.co/rest/v1/profiles","status":400,
	  "body":"{\"code\":\"22001\",\"details\":null,\"hint\":null,\"message\":\"value too long for type character varying(20)\"}"}}}`
	_ = f.pool.QueryRow(ctx, `insert into issues (project_id, fingerprint, kind, signature, title, route, first_seen_at, last_seen_at, event_count, user_count)
		values ($1, 'fp', 'net', 'net POST /rest/v1/profiles 400 pg:22001 "x"', 't', '/signup', now(), now(), 3, 2) returning id`, p.ID).Scan(&issueID)
	_, _ = f.pool.Exec(ctx, `insert into issue_samples (issue_id, event_id, received_at, payload) values ($1, 1, now(), $2)`, issueID, payload)

	_, s, err := ai.AnalyzeWith(ctx, f.pool, issueID, 1, searcher)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.FixPrompt, "Where to look in the code:\n- `src/pages/Signup.tsx` line 2: `return supabase.from('profiles').insert({ email })`") {
		t.Errorf("prompt has no code reference:\n%s", s.FixPrompt)
	}
	var refs []ai.CodeRef
	var raw []byte
	_ = f.pool.QueryRow(ctx, "select code_refs from fix_suggestions where issue_id = $1", issueID).Scan(&raw)
	_ = json.Unmarshal(raw, &refs)
	if len(refs) != 1 || refs[0].Path != "src/pages/Signup.tsx" {
		t.Errorf("stored code refs = %s", raw)
	}
}

// The manifest flow saves the new app's keys (encrypted) so a hosted
// deployment enables code context with a restart and no copying.
func TestAdminGitHubManifestSavesConfig(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	gh := http.NewServeMux()
	gh.HandleFunc("POST /app-manifests/{code}/conversions", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("code") != "one-time-code" {
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 4242, "slug": "fix-assistant-abc123", "client_id": "Iv1.abc",
			"client_secret": "shh-client-secret", "pem": pemText, "html_url": "https://github.com/apps/fix-assistant-abc123"})
	})
	ghSrv := httptest.NewServer(gh)
	defer ghSrv.Close()

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	secret := []byte("github-config-secret-github-config")
	as := &auth.Service{Pool: f.pool, Sender: f.mail, BaseURL: srv.URL}
	(&web.App{Pool: f.pool, Auth: as, Projects: &projects.Service{Pool: f.pool}, IngestBase: srv.URL, BaseURL: srv.URL,
		Secret: secret, Staff: []string{"ops@example.com"}, GitHubWeb: ghSrv.URL, GitHubAPI: ghSrv.URL,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Register(mux)
	link, err := as.AdminLink(ctx, "ops@example.com")
	if err != nil {
		t.Fatal(err)
	}
	c := client(t)
	resp, _ := c.PostForm(srv.URL+"/auth/magic", url.Values{"token": {reToken.FindStringSubmatch(link)[1]}})
	body(t, resp)

	// The admin page posts the manifest to GitHub with a signed state.
	resp, _ = c.Get(srv.URL + "/admin/github")
	// Browsers enforce form-action: the manifest form (and Connect GitHub's
	// redirect) go to github.com, so the policy must allow it.
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' https://github.com") {
		t.Errorf("CSP blocks the form to GitHub: %s", csp)
	}
	b := body(t, resp)
	m := regexp.MustCompile(`action="[^"]*/settings/apps/new\?state=([^"]+)"`).FindStringSubmatch(b)
	if m == nil {
		t.Fatalf("no manifest form:\n%s", b)
	}
	state := html.UnescapeString(m[1])

	// GitHub sends the admin back with a one-time code.
	resp, _ = c.Get(srv.URL + "/admin/github/callback?code=one-time-code&state=" + url.QueryEscape(state))
	b = body(t, resp)
	if !strings.Contains(b, "were saved") || strings.Contains(b, "BEGIN RSA PRIVATE KEY") || strings.Contains(b, "shh-client-secret") {
		t.Fatalf("callback page (%d) should confirm the save and not show keys:\n%s", resp.StatusCode, b)
	}
	// Stored encrypted; loads back with the same APP_SECRET only.
	var raw string
	_ = f.pool.QueryRow(ctx, "select encode(client_secret_enc, 'escape') || encode(private_key_enc, 'escape') from github_app_config").Scan(&raw)
	if strings.Contains(raw, "shh-client-secret") || strings.Contains(raw, "PRIVATE KEY") {
		t.Error("secrets stored in plain text")
	}
	app, err := github.LoadConfig(ctx, f.pool, secret)
	if err != nil || app == nil || app.ID != 4242 || app.Slug != "fix-assistant-abc123" || app.ClientSecret != "shh-client-secret" || app.Key.N.Cmp(key.N) != 0 {
		t.Fatalf("LoadConfig = %+v, %v", app, err)
	}
	if _, err := github.LoadConfig(ctx, f.pool, []byte("a-different-app-secret-0123456789")); err == nil {
		t.Error("loaded with the wrong APP_SECRET")
	}
	// A forged state is refused.
	resp, _ = c.Get(srv.URL + "/admin/github/callback?code=one-time-code&state=forged")
	if body(t, resp); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("forged state: %d", resp.StatusCode)
	}
}

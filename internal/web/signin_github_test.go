package web_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/yourname/fixassist/internal/auth"
	"github.com/yourname/fixassist/internal/github"
	"github.com/yourname/fixassist/internal/projects"
	"github.com/yourname/fixassist/internal/web"
)

// fakeGitHubOAuth: code "<id>-<email>" signs in GitHub user <id> whose only
// verified email is <email> ("none" = no verified email).
func fakeGitHubOAuth(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostFormValue("client_id") != "cid" || r.PostFormValue("client_secret") != "cs" || !strings.HasSuffix(r.PostFormValue("redirect_uri"), "/auth/github/callback") {
			fmt.Fprint(w, `{"error":"bad_verification_code"}`)
			return
		}
		fmt.Fprintf(w, `{"access_token":"tok-%s"}`, r.PostFormValue("code"))
	})
	who := func(r *http.Request) (string, string) {
		id, email, _ := strings.Cut(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-"), "-")
		return id, email
	}
	// "noperm:<email>": the app lacks the Email addresses permission (/user/emails
	// answers 403) and <email> is the public profile email ("" = private).
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		id, email := who(r)
		public := ""
		if p, ok := strings.CutPrefix(email, "noperm:"); ok {
			public = p
		}
		fmt.Fprintf(w, `{"id":%s,"login":"user%s","email":%q}`, id, id, public)
	})
	mux.HandleFunc("GET /user/emails", func(w http.ResponseWriter, r *http.Request) {
		_, email := who(r)
		if strings.HasPrefix(email, "noperm:") {
			w.WriteHeader(403)
			fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
			return
		}
		if email == "none" {
			fmt.Fprint(w, `[{"email":"x@example.com","primary":true,"verified":false},{"email":"1+u@users.noreply.github.com","primary":false,"verified":true}]`)
			return
		}
		fmt.Fprintf(w, `[{"email":"unverified@example.com","primary":false,"verified":false},{"email":%q,"primary":true,"verified":true}]`, email)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestSignInWithGitHub(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	gh := fakeGitHubOAuth(t)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	as := &auth.Service{Pool: f.pool, Sender: f.mail, BaseURL: srv.URL}
	(&web.App{Pool: f.pool, Auth: as, Projects: &projects.Service{Pool: f.pool}, IngestBase: srv.URL, BaseURL: srv.URL,
		Secret: []byte("signin-test-secret-signin-test-secret"),
		GitHub: &github.App{ID: 1, Slug: "fa", ClientID: "cid", ClientSecret: "cs", Key: key, APIBase: gh.URL, WebBase: gh.URL},
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil))}).Register(mux)

	resp, _ := client(t).Get(srv.URL + "/login")
	if b := body(t, resp); !strings.Contains(b, `href="/auth/github"`) || !strings.Contains(b, "Sign in with GitHub") {
		t.Fatal("no GitHub button on the login page")
	}

	// signIn starts at /auth/github in browser c and comes back with code.
	signIn := func(c *http.Client, code string) (*http.Response, string) {
		t.Helper()
		noFollow := *c
		noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := noFollow.Get(srv.URL + "/auth/github")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		loc, _ := url.Parse(resp.Header.Get("Location"))
		if resp.StatusCode != http.StatusFound || loc.Path != "/login/oauth/authorize" || loc.Query().Get("client_id") != "cid" ||
			loc.Query().Get("redirect_uri") != srv.URL+"/auth/github/callback" {
			t.Fatalf("start: %d %s", resp.StatusCode, loc)
		}
		state := loc.Query().Get("state")
		resp, err = c.Get(srv.URL + "/auth/github/callback?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(state))
		if err != nil {
			t.Fatal(err)
		}
		return resp, state
	}
	userOf := func(email string) (id, githubID int64) {
		_ = f.pool.QueryRow(ctx, "select id, coalesce(github_id, 0) from users where lower(email) = lower($1)", email).Scan(&id, &githubID)
		return
	}

	// First sign-in creates the account (and its org) and signs in.
	c := client(t)
	resp, _ = signIn(c, "777-Octo@Example.com")
	if b := body(t, resp); resp.Request.URL.Path != "/projects" || !strings.Contains(b, "Welcome to Fix Assistant") {
		t.Fatalf("after GitHub sign-in: %s (%d)", resp.Request.URL, resp.StatusCode)
	}
	id, gid := userOf("octo@example.com")
	if id == 0 || gid != 777 {
		t.Fatalf("user = %d, github id %d", id, gid)
	}
	// Again (another browser): the same account, by GitHub id.
	resp, _ = signIn(client(t), "777-octo@example.com")
	body(t, resp)
	var n int
	_ = f.pool.QueryRow(ctx, "select count(*) from users").Scan(&n)
	if n != 1 {
		t.Errorf("users = %d after signing in twice, want 1", n)
	}

	// An account made with an email link gets linked by the verified email.
	link, _ := as.AdminLink(ctx, "linked@example.com")
	emailUser, err := as.UseLink(ctx, reToken.FindStringSubmatch(link)[1])
	if err != nil {
		t.Fatal(err)
	}
	resp, _ = signIn(client(t), "888-linked@example.com")
	body(t, resp)
	if id, gid := userOf("linked@example.com"); id != emailUser || gid != 888 {
		t.Errorf("linked account = %d (github %d), want %d (888)", id, gid, emailUser)
	}
	// Another GitHub user with that email can't take the account over.
	resp, _ = signIn(client(t), "999-linked@example.com")
	if b := body(t, resp); resp.StatusCode != http.StatusConflict || !strings.Contains(b, "linked to a different GitHub account") {
		t.Errorf("takeover: %d", resp.StatusCode)
	}

	// A state from another browser (no matching cookie) is refused.
	_, state := signIn(client(t), "777-octo@example.com")
	resp, _ = client(t).Get(srv.URL + "/auth/github/callback?code=777-octo@example.com&state=" + url.QueryEscape(state))
	if b := body(t, resp); resp.StatusCode != http.StatusBadRequest || !strings.Contains(b, "another browser") {
		t.Errorf("foreign browser: %d", resp.StatusCode)
	}
	// No verified email: a clear message, no account.
	resp, _ = signIn(client(t), "555-none")
	if b := body(t, resp); !strings.Contains(b, "no verified email") {
		t.Errorf("no verified email: %d", resp.StatusCode)
	}
	if id, _ := userOf("x@example.com"); id != 0 {
		t.Error("an account was made from an unverified email")
	}
	// Without the Email addresses permission: the public profile email works,
	// a private one gets a clear message.
	resp, _ = signIn(client(t), "444-noperm:Public@Example.com")
	if b := body(t, resp); resp.Request.URL.Path != "/projects" || !strings.Contains(b, "Welcome to Fix Assistant") {
		t.Errorf("public email without permission: %s (%d)", resp.Request.URL, resp.StatusCode)
	}
	if id, gid := userOf("public@example.com"); id == 0 || gid != 444 {
		t.Errorf("public-email account = %d, github %d", id, gid)
	}
	resp, _ = signIn(client(t), "333-noperm:")
	if b := body(t, resp); !strings.Contains(b, "no verified email") {
		t.Errorf("private email without permission: %d", resp.StatusCode)
	}
	// Cancelled on GitHub.
	resp, _ = client(t).Get(srv.URL + "/auth/github/callback?error=access_denied&state=x")
	if b := body(t, resp); !strings.Contains(b, "cancelled") {
		t.Errorf("cancel: %d", resp.StatusCode)
	}
}

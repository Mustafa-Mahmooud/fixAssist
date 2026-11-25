package web_test

// Runs only with FIX_TEST_DATABASE_URL (the schema is wiped).

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/fixassist/internal/alerts"
	"github.com/yourname/fixassist/internal/auth"
	"github.com/yourname/fixassist/internal/projects"
	"github.com/yourname/fixassist/internal/store"
	"github.com/yourname/fixassist/internal/web"
)

type memSender struct {
	mu   sync.Mutex
	sent []alerts.Message
}

func (m *memSender) Send(_ context.Context, msg alerts.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, msg)
	return nil
}

func (m *memSender) last() alerts.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sent[len(m.sent)-1]
}

type fixture struct {
	pool   *pgxpool.Pool
	srv    *httptest.Server
	mail   *memSender
	authSv *auth.Service
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
	mail := &memSender{}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	as := &auth.Service{Pool: pool, Sender: mail, BaseURL: srv.URL}
	(&web.App{Pool: pool, Auth: as, Projects: &projects.Service{Pool: pool}, IngestBase: "https://ingest.example.com", BaseURL: srv.URL, Log: log}).Register(mux)
	return &fixture{pool: pool, srv: srv, mail: mail, authSv: as}
}

func client(t *testing.T) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return string(b)
}

var reToken = regexp.MustCompile(`/auth/magic\?token=([A-Za-z0-9_-]+)`)
var reCSRF = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// signIn runs the whole magic-link flow and returns a signed-in client + CSRF token.
func (f *fixture) signIn(t *testing.T, email string) (*http.Client, string) {
	t.Helper()
	c := client(t)
	resp, err := c.PostForm(f.srv.URL+"/login", url.Values{"email": {email}})
	if err != nil {
		t.Fatal(err)
	}
	if b := body(t, resp); resp.StatusCode != 200 || !strings.Contains(b, "Check your email") {
		t.Fatalf("login: %d %s", resp.StatusCode, b)
	}
	m := reToken.FindStringSubmatch(f.mail.last().Text)
	if m == nil {
		t.Fatalf("no link in email: %q", f.mail.last().Text)
	}
	// GET shows a confirm button and does not consume the token (link scanners).
	resp, _ = c.Get(f.srv.URL + "/auth/magic?token=" + m[1])
	if b := body(t, resp); !strings.Contains(b, `value="`+m[1]+`"`) {
		t.Fatalf("magic page: %s", b)
	}
	resp, err = c.PostForm(f.srv.URL+"/auth/magic", url.Values{"token": {m[1]}})
	if err != nil {
		t.Fatal(err)
	}
	b := body(t, resp)
	if resp.Request.URL.Path != "/projects" || !strings.Contains(b, "Connect your first app") {
		t.Fatalf("after sign-in: %s %s", resp.Request.URL, b)
	}
	csrf := reCSRF.FindStringSubmatch(b)
	if csrf == nil {
		t.Fatal("no csrf token on page")
	}
	// Reusing the link fails.
	resp, _ = client(t).PostForm(f.srv.URL+"/auth/magic", url.Values{"token": {m[1]}})
	if b := body(t, resp); resp.StatusCode != 400 || !strings.Contains(b, "invalid, expired or already used") {
		t.Errorf("reused token: %d", resp.StatusCode)
	}
	return c, csrf[1]
}

func TestSignInAndProjects(t *testing.T) {
	f := setup(t)
	c, csrf := f.signIn(t, "Founder@Example.com")

	// Unauthenticated requests go to /login.
	resp, _ := client(t).Get(f.srv.URL + "/projects")
	if resp.Request.URL.Path != "/login" {
		t.Errorf("anonymous /projects ended at %s", resp.Request.URL)
	}
	body(t, resp)

	// CSRF is required on POSTs.
	resp, _ = c.PostForm(f.srv.URL+"/projects", url.Values{"name": {"x"}})
	if resp.StatusCode != 403 {
		t.Errorf("no csrf: status %d", resp.StatusCode)
	}
	body(t, resp)

	// Create a project; land on setup with the prompt, the tag and the poller.
	resp, _ = c.PostForm(f.srv.URL+"/projects", url.Values{"csrf": {csrf}, "name": {"Booking app"}, "agent": {"lovable"}, "backend": {"supabase"}, "domains": {"book.lovable.app"}})
	b := body(t, resp)
	if !strings.HasSuffix(resp.Request.URL.Path, "/setup") {
		t.Fatalf("after create: %s", resp.Request.URL)
	}
	for _, want := range []string{"Ask Lovable to install it", "index.html, inside &lt;head&gt;", `data-fix-sdk src=&#34;https://ingest.example.com/sdk/`, `hx-trigger="every 3s"`, "Waiting for the first event",
		"Catch server errors too", "Add server-side error reporting", "@ingest.example.com/", "sentry_sdk.init(dsn=", "builder.WebHost.UseSentry", "Sentry.setupExpressErrorHandler", "No server errors received yet"} {
		if !strings.Contains(b, want) {
			t.Errorf("setup page missing %q", want)
		}
	}
	setupURL := resp.Request.URL.String()

	// The poll partial switches to "Connected" once an event exists.
	projectPath := strings.TrimSuffix(resp.Request.URL.Path, "/setup")
	var pid int64
	_ = f.pool.QueryRow(context.Background(), "select id from projects where name = 'Booking app'").Scan(&pid)
	if _, err := f.pool.Exec(context.Background(), "select 1"); err != nil {
		t.Fatal(err)
	}
	if err := ensurePartition(f.pool); err != nil {
		t.Fatal(err)
	}
	statusPartial := func() string {
		t.Helper()
		req, _ := http.NewRequest("GET", f.srv.URL+projectPath+"/setup/status", nil)
		req.Header.Set("HX-Request", "true")
		resp, _ := c.Do(req)
		return body(t, resp)
	}
	// A page view alone counts: a healthy site may send no errors at all.
	if _, err := f.pool.Exec(context.Background(), `insert into pageviews (project_id, route, session_id) values ($1, '/', 's1')`, pid); err != nil {
		t.Fatal(err)
	}
	if b := statusPartial(); !strings.Contains(b, "Connected.") || !strings.Contains(b, "1 page views and 0 errors") || strings.Contains(b, "hx-trigger") {
		t.Errorf("status partial after a page view: %s", b)
	}
	if _, err := f.pool.Exec(context.Background(), `insert into events (project_id, kind, fingerprint) values ($1, 'js', 'v2:x')`, pid); err != nil {
		t.Fatal(err)
	}
	if b := statusPartial(); !strings.Contains(b, "1 page views and 1 errors") {
		t.Errorf("status partial after an error: %s", b)
	}

	// Agent loop: a token is shown once, inside ready-to-paste setups.
	resp, _ = c.PostForm(f.srv.URL+projectPath+"/agent-tokens", url.Values{"csrf": {csrf}, "name": {"Cursor on my laptop"}})
	b = body(t, resp)
	token := regexp.MustCompile(`fa_[A-Za-z0-9_-]{32}`).FindString(b)
	if resp.StatusCode != 200 || token == "" {
		t.Fatalf("create token: %d, no token shown", resp.StatusCode)
	}
	for _, want := range []string{"Copy it now", "claude mcp add --transport http fix-assistant", "/mcp --header &#34;Authorization: Bearer " + token, "Cursor on my laptop", "Use the fix-assistant tools"} {
		if !strings.Contains(b, want) {
			t.Errorf("token page missing %q", want)
		}
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Error("the page showing a token must not be cached")
	}
	resp, _ = c.Get(setupURL)
	if b := body(t, resp); strings.Contains(b, token) || !strings.Contains(b, "&lt;your agent token&gt;") {
		t.Error("the token is shown again, or the placeholder is missing")
	}
	var tokenID int64
	_ = f.pool.QueryRow(context.Background(), "select id from agent_tokens where project_id = $1", pid).Scan(&tokenID)
	resp, _ = c.PostForm(fmt.Sprintf("%s%s/agent-tokens/%d/revoke", f.srv.URL, projectPath, tokenID), url.Values{"csrf": {csrf}})
	body(t, resp)
	var revoked bool
	_ = f.pool.QueryRow(context.Background(), "select revoked_at is not null from agent_tokens where id = $1", tokenID).Scan(&revoked)
	if !revoked {
		t.Error("revoke didn't revoke")
	}

	// Issues page lists grouped issues, sorted by impact.
	_, err := f.pool.Exec(context.Background(), `insert into issues (project_id, fingerprint, kind, signature, title, route, first_seen_at, last_seen_at, event_count, user_count, impact_score)
		values ($1, 'a', 'net', 's', 'POST /rest/v1/profiles returned 400: value too long', '/signup', now(), now(), 4, 2, 6),
		       ($1, 'b', 'js', 's', 'TypeError: low impact', '/', now(), now(), 1, 1, 1),
		       ($1, 'c', 'js', 's', 'Hidden one', '/', now(), now(), 1, 1, 9)`, pid)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.pool.Exec(context.Background(), `update issues set status = 'ignored' where fingerprint = 'c'`)
	resp, _ = c.Get(f.srv.URL + projectPath + "/issues")
	b = body(t, resp)
	hi, lo := strings.Index(b, "value too long"), strings.Index(b, "low impact")
	if hi < 0 || lo < 0 || hi > lo || strings.Contains(b, "Hidden one") {
		t.Errorf("issues page order/filter wrong (hi=%d lo=%d)", hi, lo)
	}
	resp, _ = c.Get(f.srv.URL + projectPath + "/issues?status=ignored")
	if b := body(t, resp); !strings.Contains(b, "Hidden one") {
		t.Error("ignored tab should show the ignored issue")
	}

	// Another founder cannot see this project.
	other, _ := f.signIn(t, "someone-else@example.com")
	for _, p := range []string{setupURL, f.srv.URL + projectPath + "/issues"} {
		resp, _ := other.Get(p)
		body(t, resp)
		if resp.StatusCode != 404 {
			t.Errorf("other org got %d for %s", resp.StatusCode, p)
		}
	}

	// Sign out ends the session.
	resp, _ = c.PostForm(f.srv.URL+"/logout", url.Values{"csrf": {csrf}})
	body(t, resp)
	resp, _ = c.Get(f.srv.URL + "/projects")
	body(t, resp)
	if resp.Request.URL.Path != "/login" {
		t.Errorf("after logout ended at %s", resp.Request.URL)
	}
}

func TestLoginRateLimitAndExpiry(t *testing.T) {
	f := setup(t)
	c := client(t)
	for i := 0; i < auth.MaxLinksPerHour; i++ {
		resp, _ := c.PostForm(f.srv.URL+"/login", url.Values{"email": {"a@example.com"}})
		body(t, resp)
	}
	resp, _ := c.PostForm(f.srv.URL+"/login", url.Values{"email": {"A@example.com"}})
	if b := body(t, resp); resp.StatusCode != 429 || !strings.Contains(b, "too many") {
		t.Errorf("6th link: %d", resp.StatusCode)
	}
	resp, _ = c.PostForm(f.srv.URL+"/login", url.Values{"email": {"not-an-email"}})
	if body(t, resp); resp.StatusCode != 400 {
		t.Errorf("bad email: %d", resp.StatusCode)
	}

	// An expired link is rejected.
	past := &auth.Service{Pool: f.pool, Sender: f.mail, BaseURL: f.srv.URL, Now: func() time.Time { return time.Now().Add(-time.Hour) }}
	if err := past.RequestLink(context.Background(), "late@example.com"); err != nil {
		t.Fatal(err)
	}
	tok := reToken.FindStringSubmatch(f.mail.last().Text)[1]
	if _, err := f.authSv.UseLink(context.Background(), tok); err != auth.ErrBadToken {
		t.Errorf("expired link: err = %v", err)
	}
}

func ensurePartition(pool *pgxpool.Pool) error {
	_, err := store.EnsurePartitions(context.Background(), pool, time.Now(), 0, 0)
	return err
}

func TestEmailOffUsesOperatorLinks(t *testing.T) {
	f := setup(t)
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	as := &auth.Service{Pool: f.pool, Sender: f.mail, BaseURL: srv.URL}
	(&web.App{Pool: f.pool, Auth: as, Projects: &projects.Service{Pool: f.pool}, IngestBase: srv.URL, BaseURL: srv.URL, EmailOff: true, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Register(mux)

	c := client(t)
	resp, _ := c.Get(srv.URL + "/login")
	if b := body(t, resp); !strings.Contains(b, "switched on yet") || strings.Contains(b, `name="email"`) {
		t.Errorf("login page with email off: %s", b)
	}
	resp, _ = c.PostForm(srv.URL+"/login", url.Values{"email": {"x@example.com"}})
	if body(t, resp); resp.StatusCode != http.StatusServiceUnavailable || len(f.mail.sent) != 0 {
		t.Errorf("POST /login with email off: %d, %d emails", resp.StatusCode, len(f.mail.sent))
	}

	link, err := as.AdminLink(context.Background(), "Partner@Example.com")
	if err != nil {
		t.Fatal(err)
	}
	tok := reToken.FindStringSubmatch(link)[1]
	resp, _ = c.PostForm(srv.URL+"/auth/magic", url.Values{"token": {tok}})
	if b := body(t, resp); resp.Request.URL.Path != "/projects" || !strings.Contains(b, "partner@example.com") {
		t.Errorf("operator link sign-in ended at %s", resp.Request.URL)
	}
}

func TestFeedbackLinksAndButtons(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	secret := []byte("test-secret-test-secret-test-secret")
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	as := &auth.Service{Pool: f.pool, Sender: f.mail, BaseURL: srv.URL}
	(&web.App{Pool: f.pool, Auth: as, Projects: &projects.Service{Pool: f.pool}, IngestBase: srv.URL, BaseURL: srv.URL,
		Secret: secret, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Register(mux)

	p, err := (&projects.Service{Pool: f.pool}).Create(ctx, projects.CreateParams{Org: "acme", Name: "app", Agent: "lovable", Backend: "supabase"})
	if err != nil {
		t.Fatal(err)
	}
	var issueID, sid int64
	_ = f.pool.QueryRow(ctx, `insert into issues (project_id, fingerprint, kind, signature, title, route, status, first_seen_at, last_seen_at, event_count)
		values ($1, 'fp', 'net', 'net POST /rest/v1/profiles 400 pg:22001', 'Saving fails', '/', 'explained', now(), now(), 3) returning id`, p.ID).Scan(&issueID)
	_ = f.pool.QueryRow(ctx, `insert into fix_suggestions (issue_id, origin, prompt_version, target_agent, severity, title, what_broke, who_is_affected, fix_prompt, how_to_check)
		values ($1, 'catalog', 'v1', 'lovable', 'high', 'Saving to profiles fails', 'w', 'w', 'p', 'c') returning id`, issueID).Scan(&sid)

	link := web.FeedbackLink(srv.URL, secret, sid, "failed", time.Now())
	c := client(t)
	// GET only shows a confirm button.
	resp, _ := c.Get(link)
	if b := body(t, resp); resp.StatusCode != 200 || !strings.Contains(b, "still broken") {
		t.Fatalf("confirm page: %d %s", resp.StatusCode, b)
	}
	if n := countRows(t, f.pool, "select count(*) from fix_feedback"); n != 0 {
		t.Fatal("GET recorded feedback")
	}
	// Tampered and expired links are refused.
	for _, bad := range []string{strings.Replace(link, "/failed?", "/worked?", 1), web.FeedbackLink(srv.URL, secret, sid, "failed", time.Now().Add(-31*24*time.Hour))} {
		resp, _ := c.PostForm(strings.Split(bad, "?")[0], parseQuery(t, bad))
		if body(t, resp); resp.StatusCode != 400 {
			t.Errorf("bad link accepted: %d", resp.StatusCode)
		}
	}
	// POST records "didn't work", reopens and writes a second attempt.
	resp, _ = c.PostForm(strings.Split(link, "?")[0], parseQuery(t, link))
	if b := body(t, resp); resp.StatusCode != 200 || !strings.Contains(b, "new prompt") {
		t.Fatalf("submit: %d %s", resp.StatusCode, b)
	}
	if n := countRows(t, f.pool, "select count(*) from fix_feedback where outcome = 'failed' and source = 'explicit'"); n != 1 {
		t.Errorf("failed feedback rows = %d", n)
	}
	if n := countRows(t, f.pool, "select count(*) from fix_suggestions where issue_id = $1 and attempt = 2", issueID); n != 1 {
		t.Errorf("second-attempt suggestions = %d", n)
	}
	var status string
	_ = f.pool.QueryRow(ctx, "select status from issues where id = $1", issueID).Scan(&status)
	if status != "open" {
		t.Errorf("status = %s, want open", status)
	}
}

func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func parseQuery(t *testing.T, link string) url.Values {
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func TestAdminIsStaffOnly(t *testing.T) {
	f := setup(t)
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	as := &auth.Service{Pool: f.pool, Sender: f.mail, BaseURL: srv.URL}
	(&web.App{Pool: f.pool, Auth: as, Projects: &projects.Service{Pool: f.pool}, IngestBase: srv.URL, BaseURL: srv.URL,
		Staff: []string{"ops@example.com"}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Register(mux)
	signIn := func(email string) *http.Client {
		link, err := as.AdminLink(context.Background(), email)
		if err != nil {
			t.Fatal(err)
		}
		c := client(t)
		resp, _ := c.PostForm(srv.URL+"/auth/magic", url.Values{"token": {reToken.FindStringSubmatch(link)[1]}})
		body(t, resp)
		return c
	}
	staff, founder := signIn("Ops@Example.com"), signIn("founder@example.com")
	resp, _ := staff.Get(srv.URL + "/admin")
	if b := body(t, resp); resp.StatusCode != 200 || !strings.Contains(b, "Fix prompts: did they work?") {
		t.Errorf("staff /admin: %d", resp.StatusCode)
	}
	resp, _ = founder.Get(srv.URL + "/admin")
	if body(t, resp); resp.StatusCode != 404 {
		t.Errorf("founder /admin: %d, want 404", resp.StatusCode)
	}
	resp, _ = founder.Get(srv.URL + "/projects")
	if b := body(t, resp); strings.Contains(b, `href="/admin"`) {
		t.Error("admin link shown to a non-staff user")
	}
}

func TestIssuePageShowsTrackRecord(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	c, _ := f.signIn(t, "kb@example.com")
	var orgID, pid, issueID, kbID int64
	_ = f.pool.QueryRow(ctx, "select m.org_id from memberships m join users u on u.id = m.user_id where u.email = 'kb@example.com'").Scan(&orgID)
	p, err := (&projects.Service{Pool: f.pool}).Create(ctx, projects.CreateParams{OrgID: orgID, Name: "kb", Agent: "lovable", Backend: "supabase"})
	if err != nil {
		t.Fatal(err)
	}
	pid = p.ID
	sig := `net POST /rest/v1/profiles 400 pg:22001 "value too long for type character varying(20)"`
	_ = f.pool.QueryRow(ctx, `insert into knowledge_entries (signature, error_type, explanation_template, worked, failed, confidence)
		values ($1, 'net', 'v1:pg.value_too_long', 4, 1, 0.714) returning id`, sig).Scan(&kbID)
	_, _ = f.pool.Exec(ctx, `insert into knowledge_entries (signature, error_type, explanation_template, worked, failed)
		values ('net POST /rest/v1/members 400 pg:22001 "value too long for type character varying(40)"', 'net', 'v1:x', 3, 0)`)
	_, _ = f.pool.Exec(ctx, `insert into kb_sightings (entry_id, project_hash) values ($1, 'a'), ($1, 'b'), ($1, 'c')`, kbID)
	_ = f.pool.QueryRow(ctx, `insert into issues (project_id, fingerprint, kind, signature, title, route, status, first_seen_at, last_seen_at, event_count)
		values ($1, 'fp', 'net', $2 || ' platform=lovable', 'Saving fails', '/', 'explained', now(), now(), 3) returning id`, pid, sig).Scan(&issueID)
	_, _ = f.pool.Exec(ctx, `insert into fix_suggestions (issue_id, origin, prompt_version, target_agent, severity, title, what_broke, who_is_affected, fix_prompt, how_to_check, kb_entry_id)
		values ($1, 'cache', 'v1', 'lovable', 'high', 't', 'w', 'w', 'p', 'c', $2)`, issueID, kbID)

	resp, _ := c.Get(fmt.Sprintf("%s/projects/%d/issues/%d", f.srv.URL, pid, issueID))
	b := body(t, resp)
	for _, want := range []string{"Proven fix", "This fix worked 4 of 5 times across 3 apps", "Similar errors in other apps were fixed 3 of 3 times"} {
		if !strings.Contains(b, want) {
			t.Errorf("issue page missing %q (status %d)", want, resp.StatusCode)
		}
	}
	if strings.Contains(b, "/rest/v1/members") {
		t.Error("another app's signature leaked onto the page")
	}
}

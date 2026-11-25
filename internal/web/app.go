package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/fixassist/internal/agentapi"
	"github.com/yourname/fixassist/internal/ai"
	"github.com/yourname/fixassist/internal/auth"
	"github.com/yourname/fixassist/internal/github"
	"github.com/yourname/fixassist/internal/obs"
	"github.com/yourname/fixassist/internal/projects"
	"github.com/yourname/fixassist/internal/store/db"
	"github.com/yourname/fixassist/internal/verify"
)

const sessionCookie = "fix_session"

// App is the founder dashboard (FDR: Go templates + htmx, served by the binary).
type App struct {
	Pool       *pgxpool.Pool
	Auth       *auth.Service
	Projects   *projects.Service
	IngestBase string       // for script tags
	BaseURL    string       // the dashboard's own public URL
	EmailOff   bool         // no email provider: the login page asks for an operator-issued link
	Secret     []byte       // signs the feedback links in alert emails (APP_SECRET)
	Staff      []string     // lowercase emails allowed into /admin (STAFF_EMAILS)
	Metrics    *obs.Metrics // optional, for the admin page
	GitHub     *github.App  // optional: code context (Phase 7)
	Code       ai.CodeSearcher
	GitHubWeb  string // overrides https://github.com (tests)
	GitHubAPI  string // overrides https://api.github.com (tests)
	Log        *slog.Logger
}

type ctxKey struct{}

func sessionFrom(r *http.Request) *auth.Session {
	s, _ := r.Context().Value(ctxKey{}).(*auth.Session)
	return s
}

// Register mounts every dashboard route.
func (a *App) Register(mux *http.ServeMux) {
	mux.Handle("GET /static/", staticHandler())
	mux.HandleFunc("GET /login", a.loginPage)
	mux.HandleFunc("POST /login", a.loginSubmit)
	mux.HandleFunc("GET /auth/magic", a.magicPage)
	mux.HandleFunc("POST /auth/magic", a.magicSubmit)
	mux.Handle("POST /logout", a.signedIn(a.logout))
	mux.Handle("GET /{$}", a.signedIn(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/projects", http.StatusSeeOther) }))
	mux.Handle("GET /projects", a.signedIn(a.projectsPage))
	mux.Handle("POST /projects", a.signedIn(a.createProject))
	mux.Handle("GET /projects/{id}/setup", a.signedIn(a.setupPage))
	mux.Handle("GET /projects/{id}/setup/status", a.signedIn(a.setupStatus))
	mux.Handle("POST /projects/{id}/agent-tokens", a.signedIn(a.createAgentToken))
	mux.Handle("POST /projects/{id}/agent-tokens/{token}/revoke", a.signedIn(a.revokeAgentToken))
	mux.Handle("GET /projects/{id}/issues", a.signedIn(a.issuesPage))
	mux.Handle("GET /projects/{id}/issues/{issue}", a.signedIn(a.issuePage))
	mux.Handle("POST /projects/{id}/issues/{issue}/status", a.signedIn(a.setIssueStatus))
	mux.Handle("POST /projects/{id}/issues/{issue}/explain", a.signedIn(a.explainNow))
	mux.Handle("GET /projects/{id}/alerts", a.signedIn(a.alertsPage))
	mux.Handle("GET /projects/{id}/overview", a.signedIn(a.overviewPage))
	mux.Handle("GET /projects/{id}/pages", a.signedIn(a.pagesPage))
	mux.Handle("GET /projects/{id}/funnels", a.signedIn(a.funnelsPage))
	mux.Handle("GET /projects/{id}/insights", a.signedIn(a.insightsPage))
	mux.Handle("POST /projects/{id}/insights/generate", a.signedIn(a.generateInsights))
	mux.Handle("POST /projects/{id}/insights/{insight}/outcome", a.signedIn(a.insightOutcome))
	mux.Handle("POST /projects/{id}/funnels", a.signedIn(a.createFunnel))
	mux.Handle("POST /projects/{id}/goals", a.signedIn(a.createGoal))
	mux.Handle("POST /projects/{id}/{what}/{item}/delete", a.signedIn(a.deleteFunnelOrGoal))
	mux.Handle("POST /projects/{id}/issues/{issue}/feedback", a.signedIn(a.issueFeedback))
	mux.Handle("POST /projects/{id}/issues/{issue}/applied", a.signedIn(a.issueApplied))
	mux.Handle("GET /admin", a.staffOnly(a.adminPage))
	mux.Handle("GET /admin/issues/{id}", a.staffOnly(a.adminIssue))
	mux.Handle("POST /admin/kb/{id}/reset", a.staffOnly(a.adminUnquarantine))
	mux.Handle("GET /admin/github", a.staffOnly(a.adminGitHub))
	mux.Handle("GET /admin/github/callback", a.staffOnly(a.adminGitHubCallback))
	mux.Handle("POST /projects/{id}/github/connect", a.signedIn(a.githubConnect))
	mux.Handle("GET /github/setup", a.signedIn(a.githubSetup))
	mux.Handle("POST /projects/{id}/github/repo", a.signedIn(a.githubChooseRepo))
	mux.Handle("POST /projects/{id}/github/disconnect", a.signedIn(a.githubDisconnect))
	mux.HandleFunc("GET /f/{sid}/{outcome}", a.feedbackLinkPage)
	mux.HandleFunc("POST /f/{sid}/{outcome}", a.feedbackLinkSubmit)
}

func (a *App) secureCookies() bool { return strings.HasPrefix(a.BaseURL, "https://") }

func securityHeaders(w http.ResponseWriter) {
	h := w.Header()
	// form-action also allows GitHub: the GitHub App manifest form posts to
	// github.com, and "Connect GitHub" redirects there after its POST.
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self' https://github.com")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
}

// signedIn loads the session, redirects to /login when missing, and checks
// the CSRF token on every POST (form field "csrf" or X-CSRF-Token header).
func (a *App) signedIn(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w)
		var cookie string
		if c, err := r.Cookie(sessionCookie); err == nil {
			cookie = c.Value
		}
		sess, ok, err := a.Auth.LoadSession(r.Context(), cookie)
		if err != nil {
			a.Log.Error("load session", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !ok {
			if r.Header.Get("HX-Request") != "" {
				w.Header().Set("HX-Redirect", "/login")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost {
			tok := r.Header.Get("X-CSRF-Token")
			if tok == "" {
				tok = r.PostFormValue("csrf")
			}
			if !auth.CSRFValid(sess, tok) {
				http.Error(w, "invalid or missing CSRF token; reload the page", http.StatusForbidden)
				return
			}
		}
		sess.Staff = slices.Contains(a.Staff, strings.ToLower(sess.Email))
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, &sess)))
	})
}

// --- auth pages ---------------------------------------------------------

type loginData struct {
	Email    string
	Sent     bool
	EmailOff bool
}

func (a *App) loginPage(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w)
	a.render(w, 200, "login", page{Title: "Sign in", Data: loginData{EmailOff: a.EmailOff}})
}

func (a *App) loginSubmit(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w)
	if a.EmailOff {
		a.render(w, http.StatusServiceUnavailable, "login", page{Title: "Sign in", Data: loginData{EmailOff: true}})
		return
	}
	email := r.PostFormValue("email")
	err := a.Auth.RequestLink(r.Context(), email)
	switch {
	case errors.Is(err, auth.ErrBadEmail), errors.Is(err, auth.ErrRateLimited):
		status := http.StatusBadRequest
		if errors.Is(err, auth.ErrRateLimited) {
			status = http.StatusTooManyRequests
		}
		a.render(w, status, "login", page{Title: "Sign in", Error: err.Error(), Data: loginData{Email: email}})
	case err != nil:
		a.Log.Error("request link", "err", err)
		a.render(w, 500, "login", page{Title: "Sign in", Error: "We couldn't send the email. Please try again.", Data: loginData{Email: email}})
	default:
		a.render(w, 200, "login", page{Title: "Check your email", Data: loginData{Email: email, Sent: true}})
	}
}

func (a *App) magicPage(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w)
	w.Header().Set("Referrer-Policy", "no-referrer") // the token is in the URL
	a.render(w, 200, "magic", page{Title: "Sign in", Data: map[string]string{"Token": r.URL.Query().Get("token")}})
}

func (a *App) magicSubmit(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w)
	userID, err := a.Auth.UseLink(r.Context(), r.PostFormValue("token"))
	if errors.Is(err, auth.ErrBadToken) {
		a.render(w, 400, "magic", page{Title: "Sign in", Error: err.Error()})
		return
	}
	if err != nil {
		a.Log.Error("use link", "err", err)
		http.Error(w, "internal error", 500)
		return
	}
	cookie, err := a.Auth.CreateSession(r.Context(), userID)
	if err != nil {
		a.Log.Error("create session", "err", err)
		http.Error(w, "internal error", 500)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: cookie, Path: "/", HttpOnly: true, Secure: a.secureCookies(),
		SameSite: http.SameSiteLaxMode, MaxAge: int(auth.SessionTTL.Seconds()),
	})
	http.Redirect(w, r, "/projects", http.StatusSeeOther)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = a.Auth.EndSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.secureCookies(), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- projects -------------------------------------------------------------

func (a *App) projectsPage(w http.ResponseWriter, r *http.Request) {
	a.renderProjects(w, r, 200, "")
}

func (a *App) renderProjects(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	s := sessionFrom(r)
	list, err := db.New(a.Pool).ListOrgProjects(r.Context(), s.OrgID)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.render(w, status, "projects", page{Title: "Projects", Session: s, Error: errMsg, Data: map[string]any{
		"Projects": list, "Agents": projects.Agents,
	}})
}

func (a *App) createProject(w http.ResponseWriter, r *http.Request) {
	s := sessionFrom(r)
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" || len(name) > 80 {
		a.renderProjects(w, r, 400, "Give your app a name (up to 80 characters).")
		return
	}
	var domains []string
	for _, d := range strings.FieldsFunc(r.PostFormValue("domains"), func(c rune) bool { return c == ',' || c == ' ' }) {
		domains = append(domains, d)
	}
	p, err := a.Projects.Create(r.Context(), projects.CreateParams{
		OrgID: s.OrgID, Name: name, Agent: r.PostFormValue("agent"), Backend: r.PostFormValue("backend"), AllowedDomains: domains,
	})
	if err != nil {
		a.renderProjects(w, r, 400, err.Error())
		return
	}
	http.Redirect(w, r, "/projects/"+strconv.FormatInt(p.ID, 10)+"/setup", http.StatusSeeOther)
}

// project loads a project of the signed-in user's org, or writes 404.
func (a *App) project(w http.ResponseWriter, r *http.Request) (db.GetOrgProjectRow, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return db.GetOrgProjectRow{}, false
	}
	p, err := db.New(a.Pool).GetOrgProject(r.Context(), db.GetOrgProjectParams{ID: id, OrgID: sessionFrom(r).OrgID})
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return p, false
	}
	if err != nil {
		a.fail(w, err)
		return p, false
	}
	return p, true
}

func (a *App) fail(w http.ResponseWriter, err error) {
	a.Log.Error("dashboard", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// --- setup ----------------------------------------------------------------

type setupStatus struct {
	ProjectID    int64
	Events       int64 // errors from the browser and the server
	ServerEvents int64
	PageViews    int64
	Connected    bool
	LastEventAt  time.Time
}

func (a *App) status(ctx context.Context, projectID int64) (setupStatus, error) {
	st, err := db.New(a.Pool).ProjectEventStats(ctx, projectID)
	return setupStatus{ProjectID: projectID, Events: st.Events, ServerEvents: st.ServerEvents, PageViews: st.PageViews,
		Connected: st.Events+st.PageViews > 0, LastEventAt: st.LastEventAt.Time}, err
}

func (a *App) setupPage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok {
		return
	}
	a.renderSetup(w, r, p, "")
}

// renderSetup renders the Setup page; newToken is an agent token to show
// once, right after it was created.
func (a *App) renderSetup(w http.ResponseWriter, r *http.Request, p db.GetOrgProjectRow, newToken string) {
	st, err := a.status(r.Context(), p.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	tag := ScriptTag(a.IngestBase, p.PublicKey)
	dsn, err := projects.DSN(a.IngestBase, p.PublicKey, p.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	tokens, err := db.New(a.Pool).ListAgentTokens(r.Context(), p.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	shown := newToken
	if shown == "" {
		shown = "<your agent token>"
	}
	mcpURL := strings.TrimRight(a.BaseURL, "/") + "/mcp"
	a.render(w, 200, "setup", page{Title: "Setup", Session: sessionFrom(r), Data: map[string]any{
		"Project": p, "ScriptTag": tag, "Prompt": InstallPrompt(p.TargetAgent, tag), "Status": st,
		"GitHub": a.githubStatus(r, p.ID),
		"DSN":    dsn, "ServerPrompt": ServerPrompt(dsn), "ServerSnippets": ServerSnippets(dsn),
		"AgentTokens": tokens, "NewToken": newToken, "MCPURL": mcpURL,
		"AgentSetups": AgentSetups(mcpURL, shown), "LoopPrompt": LoopPrompt(), "MaxAttempts": verify.MaxAttempts,
	}})
}

// createAgentToken makes a token for the agent loop and shows it once.
func (a *App) createAgentToken(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		name = "AI agent"
	}
	if len(name) > 60 {
		name = name[:60]
	}
	token, hash, prefix, err := agentapi.NewToken()
	if err != nil {
		a.fail(w, err)
		return
	}
	if _, err := db.New(a.Pool).CreateAgentToken(r.Context(), db.CreateAgentTokenParams{
		ProjectID: p.ID, Name: name, TokenHash: hash, Prefix: prefix,
		CreatedBy: pgtype.Int8{Int64: sessionFrom(r).UserID, Valid: true},
	}); err != nil {
		a.fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	a.renderSetup(w, r, p, token)
}

func (a *App) revokeAgentToken(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("token"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := db.New(a.Pool).RevokeAgentToken(r.Context(), db.RevokeAgentTokenParams{ID: id, ProjectID: p.ID}); err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, "/projects/"+strconv.FormatInt(p.ID, 10)+"/setup#agent-loop", http.StatusSeeOther)
}

func (a *App) setupStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok {
		return
	}
	st, err := a.status(r.Context(), p.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.partial(w, "setup_status", st)
}

// --- issues ---------------------------------------------------------------

type tab struct{ Value, Label string }

var issueTabs = []tab{{"active", "Needs attention"}, {"new", "New"}, {"open", "Came back"}, {"fixed", "Fixed"}, {"ignored", "Ignored"}, {"", "All"}}

func (a *App) issuesPage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok {
		return
	}
	status := r.URL.Query().Get("status")
	if _, set := r.URL.Query()["status"]; !set {
		status = "active"
	}
	valid := false
	for _, t := range issueTabs {
		valid = valid || t.Value == status
	}
	if !valid {
		status = "active"
	}
	issues, err := db.New(a.Pool).ListIssues(r.Context(), db.ListIssuesParams{ProjectID: p.ID, Status: status})
	if err != nil {
		a.fail(w, err)
		return
	}
	a.render(w, 200, "issues", page{Title: "Issues", Session: sessionFrom(r), Data: map[string]any{
		"Project": p, "Issues": issues, "Status": status, "Tabs": issueTabs,
	}})
}

func (a *App) issue(w http.ResponseWriter, r *http.Request, projectID int64) (db.Issue, bool) {
	id, err := strconv.ParseInt(r.PathValue("issue"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return db.Issue{}, false
	}
	is, err := db.New(a.Pool).GetIssue(r.Context(), db.GetIssueParams{ID: id, ProjectID: projectID})
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return is, false
	}
	if err != nil {
		a.fail(w, err)
		return is, false
	}
	return is, true
}

func (a *App) issuePage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok {
		return
	}
	is, ok := a.issue(w, r, p.ID)
	if !ok {
		return
	}
	q := db.New(a.Pool)
	since := time.Now().UTC().Truncate(time.Hour).Add(-167 * time.Hour)
	hourly, err := q.IssueHourly(r.Context(), db.IssueHourlyParams{IssueID: is.ID, Hour: pgtype.Timestamptz{Time: since, Valid: true}})
	if err != nil {
		a.fail(w, err)
		return
	}
	counts := make([]int, 168)
	for _, h := range hourly {
		if i := int(h.Hour.Time.Sub(since) / time.Hour); i >= 0 && i < len(counts) {
			counts[i] = int(h.Events)
		}
	}
	samples, err := q.IssueSamples(r.Context(), db.IssueSamplesParams{IssueID: is.ID, Limit: 5})
	if err != nil {
		a.fail(w, err)
		return
	}
	data := map[string]any{
		"Project": p, "Issue": is, "Samples": samples, "Spark": sparkline(counts, "events per hour, last 7 days"),
		"Agents": projects.Agents, "Agent": p.TargetAgent,
	}
	// The founder can switch the target agent on any issue (FDR); in agent
	// mode the prompt is re-rendered, nothing is regenerated.
	if ag := r.URL.Query().Get("agent"); slices.Contains(projects.Agents, ag) {
		data["Agent"] = ag
	}
	fb, err := q.IssueFeedback(r.Context(), is.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	data["Feedback"] = fb
	attempts, err := q.FixAttempts(r.Context(), is.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	data["Attempts"] = attempts
	if is.Status == "verifying" {
		data["Verify"] = a.verifyProgress(r, p.ID, is)
	}
	sug, err := q.LatestSuggestion(r.Context(), is.ID)
	switch {
	case err == nil:
		data["Suggestion"] = sug
		data["Prompt"] = ai.PromptForRow(data["Agent"].(string), sug)
		// Track record across apps (Phase 7). Other apps are only ever shown
		// as totals, never their signatures.
		if sug.KbEntryID.Valid {
			if rec, err := q.KBTrackRecord(r.Context(), sug.KbEntryID.Int64); err == nil && rec.Worked+rec.Failed > 0 {
				data["Track"] = rec
			}
		}
		if sim, err := q.KBSimilar(r.Context(), db.KBSimilarParams{Signature: is.Signature, MinSimilarity: ai.SimilarMinScore}); err == nil && sim.Worked+sim.Failed > 0 {
			data["Similar"] = sim
		}
	case !errors.Is(err, pgx.ErrNoRows):
		a.fail(w, err)
		return
	}
	a.render(w, 200, "issue", page{Title: "Issue", Session: sessionFrom(r), Data: data})
}

// explainNow is the FDR "explain now" button: analyze below the threshold,
// or regenerate. Agent mode is cheap and synchronous.
func (a *App) explainNow(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok {
		return
	}
	is, ok := a.issue(w, r, p.ID)
	if !ok {
		return
	}
	if _, _, err := ai.AnalyzeWith(r.Context(), a.Pool, is.ID, ai.AttemptAuto, a.Code); err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, "/projects/"+strconv.FormatInt(p.ID, 10)+"/issues/"+strconv.FormatInt(is.ID, 10), http.StatusSeeOther)
}

func (a *App) alertsPage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok {
		return
	}
	list, err := db.New(a.Pool).ListAlerts(r.Context(), p.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.render(w, 200, "alerts", page{Title: "Alerts", Session: sessionFrom(r), Data: map[string]any{"Project": p, "Alerts": list}})
}

func (a *App) setIssueStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok {
		return
	}
	is, ok := a.issue(w, r, p.ID)
	if !ok {
		return
	}
	// Phase 3 lets founders hide or unhide issues; fix states come from Phases 4-5.
	status := r.PostFormValue("status")
	if status != "ignored" && status != "open" {
		http.Error(w, "bad status", http.StatusBadRequest)
		return
	}
	if _, err := db.New(a.Pool).SetIssueStatus(r.Context(), db.SetIssueStatusParams{ID: is.ID, ProjectID: p.ID, Status: status}); err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, "/projects/"+strconv.FormatInt(p.ID, 10)+"/issues/"+strconv.FormatInt(is.ID, 10), http.StatusSeeOther)
}

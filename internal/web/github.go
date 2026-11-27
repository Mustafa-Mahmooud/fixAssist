package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yourname/fixassist/internal/github"
	"github.com/yourname/fixassist/internal/store/db"
)

// GitHub App code context (FDR Phase 7). Connecting a repo is optional; it
// lets fix prompts name the exact file and line.

// signState makes a tamper-proof, expiring state value for GitHub redirects.
func (a *App) signState(kind string, parts ...string) string {
	payload := strings.Join(append([]string{kind, strconv.FormatInt(time.Now().Add(30*time.Minute).Unix(), 10)}, parts...), ".")
	m := hmac.New(sha256.New, a.Secret)
	m.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// checkState returns the parts after kind and expiry, if valid.
func (a *App) checkState(state, kind string) ([]string, bool) {
	i := strings.LastIndexByte(state, '.')
	if i < 0 {
		return nil, false
	}
	payload, sig := state[:i], state[i+1:]
	m := hmac.New(sha256.New, a.Secret)
	m.Write([]byte(payload))
	if !hmac.Equal([]byte(base64.RawURLEncoding.EncodeToString(m.Sum(nil))), []byte(sig)) {
		return nil, false
	}
	parts := strings.Split(payload, ".")
	if len(parts) < 2 || parts[0] != kind {
		return nil, false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return nil, false
	}
	return parts[2:], true
}

// githubStatus is shown on the setup page.
type githubStatus struct {
	Enabled bool
	Repo    string
	Choose  []string // repos to pick from after installing
}

func (a *App) githubStatus(r *http.Request, projectID int64) githubStatus {
	if a.GitHub == nil {
		return githubStatus{}
	}
	st := githubStatus{Enabled: true}
	g, err := db.New(a.Pool).GetProjectGitHub(r.Context(), projectID)
	if err != nil {
		return st
	}
	if g.RepoFullName.Valid {
		st.Repo = g.RepoFullName.String
		return st
	}
	if repos, err := a.GitHub.Repos(r.Context(), g.InstallationID); err == nil {
		st.Choose = repos
	}
	return st
}

func (a *App) githubConnect(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok || a.GitHub == nil {
		http.NotFound(w, r)
		return
	}
	state := a.signState("gh", strconv.FormatInt(p.ID, 10), strconv.FormatInt(sessionFrom(r).UserID, 10))
	http.Redirect(w, r, a.GitHub.InstallURL(state), http.StatusSeeOther)
}

// githubSetup is where GitHub sends the founder after installing the app.
func (a *App) githubSetup(w http.ResponseWriter, r *http.Request) {
	if a.GitHub == nil {
		http.NotFound(w, r)
		return
	}
	parts, ok := a.checkState(r.URL.Query().Get("state"), "gh")
	s := sessionFrom(r)
	if !ok || len(parts) != 2 || parts[1] != strconv.FormatInt(s.UserID, 10) {
		http.Error(w, "This GitHub link is invalid or expired. Start again from your project's Setup page.", http.StatusBadRequest)
		return
	}
	projectID, _ := strconv.ParseInt(parts[0], 10, 64)
	if _, err := a.accessProject(r.Context(), s, projectID); err != nil {
		http.NotFound(w, r)
		return
	}
	installationID, err := strconv.ParseInt(r.URL.Query().Get("installation_id"), 10, 64)
	if err != nil {
		http.Error(w, "missing installation", http.StatusBadRequest)
		return
	}
	// The installation must be one this GitHub user can access.
	allowed, err := a.GitHub.UserCanAccess(r.Context(), r.URL.Query().Get("code"), installationID)
	if err != nil {
		a.Log.Warn("github oauth", "err", err)
		http.Error(w, "GitHub didn't confirm the installation. Please try again.", http.StatusBadGateway)
		return
	}
	if !allowed {
		http.Error(w, "That GitHub installation doesn't belong to your GitHub account.", http.StatusForbidden)
		return
	}
	q := db.New(a.Pool)
	if err := q.ConnectProjectGitHub(r.Context(), db.ConnectProjectGitHubParams{
		ProjectID: projectID, InstallationID: installationID, ConnectedBy: pgtype.Int8{Int64: s.UserID, Valid: true},
	}); err != nil {
		a.fail(w, err)
		return
	}
	if repos, err := a.GitHub.Repos(r.Context(), installationID); err == nil && len(repos) == 1 {
		_, _ = q.SetProjectRepo(r.Context(), db.SetProjectRepoParams{ProjectID: projectID, RepoFullName: pgtype.Text{String: repos[0], Valid: true}})
	}
	http.Redirect(w, r, fmt.Sprintf("/projects/%d/setup#code", projectID), http.StatusSeeOther)
}

func (a *App) githubChooseRepo(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok || a.GitHub == nil {
		http.NotFound(w, r)
		return
	}
	q := db.New(a.Pool)
	g, err := q.GetProjectGitHub(r.Context(), p.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	repo := r.PostFormValue("repo")
	repos, err := a.GitHub.Repos(r.Context(), g.InstallationID)
	if err != nil || !slices.Contains(repos, repo) {
		http.Error(w, "that repository isn't part of the installation", http.StatusBadRequest)
		return
	}
	if _, err := q.SetProjectRepo(r.Context(), db.SetProjectRepoParams{ProjectID: p.ID, RepoFullName: pgtype.Text{String: repo, Valid: true}}); err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/projects/%d/setup#code", p.ID), http.StatusSeeOther)
}

func (a *App) githubDisconnect(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok {
		return
	}
	if err := db.New(a.Pool).DisconnectProjectGitHub(r.Context(), p.ID); err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/projects/%d/setup#code", p.ID), http.StatusSeeOther)
}

// --- staff: create the GitHub App from a manifest --------------------------

func (a *App) adminGitHub(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"Configured": a.GitHub != nil}
	if a.GitHub != nil {
		data["Slug"], data["AppID"] = a.GitHub.Slug, a.GitHub.ID
	} else {
		web := "https://github.com"
		if a.GitHubWeb != "" {
			web = a.GitHubWeb
		}
		data["Action"] = web + "/settings/apps/new?state=" + a.signState("ghapp")
		data["Manifest"] = string(github.Manifest(a.BaseURL))
	}
	a.render(w, 200, "admin_github", page{Title: "GitHub App", Session: sessionFrom(r), Data: data})
}

func (a *App) adminGitHubCallback(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.checkState(r.URL.Query().Get("state"), "ghapp"); !ok {
		http.Error(w, "invalid or expired state", http.StatusBadRequest)
		return
	}
	conv, err := github.Convert(r.Context(), a.GitHubAPI, r.URL.Query().Get("code"))
	if err != nil {
		a.Log.Warn("github manifest conversion", "err", err)
		http.Error(w, "GitHub had an error and didn't send the new app's keys. Open https://github.com/settings/apps: if an app named \"Fix Assistant ...\" is there, delete it (Advanced > Delete), then try again from /admin/github.", http.StatusBadGateway)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	// Save the keys (encrypted) so the next start enables code context with
	// no copying; show them only if saving fails.
	if err := github.SaveConfig(r.Context(), a.Pool, a.Secret, conv); err != nil {
		a.Log.Error("github app config not saved", "err", err)
		a.render(w, 200, "admin_github", page{Title: "GitHub App", Session: sessionFrom(r), Data: map[string]any{"Created": conv}})
		return
	}
	a.Log.Info("github app created and saved", "slug", conv.Slug, "app_id", conv.ID)
	a.render(w, 200, "admin_github", page{Title: "GitHub App", Session: sessionFrom(r), Data: map[string]any{"Saved": conv}})
}

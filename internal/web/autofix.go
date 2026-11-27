package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yourname/fixassist/internal/autofix"
	"github.com/yourname/fixassist/internal/store/db"
)

// Auto-fix on the dashboard: the project's switch and rule (Setup page), the
// run on an issue page, and the signed "Auto-fix this" link in emails.

// AutofixLinkTTL matches how long an approval request stays open.
const AutofixLinkTTL = autofix.ApprovalTTL

func autofixSig(secret []byte, runID, exp int64) string {
	m := hmac.New(sha256.New, secret)
	fmt.Fprintf(m, "autofix|%d|%d", runID, exp)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// AutofixLink builds the signed approval link for an auto-fix email.
func AutofixLink(baseURL string, secret []byte, runID int64, now time.Time) string {
	exp := now.Add(AutofixLinkTTL).Unix()
	return fmt.Sprintf("%s/a/%d?e=%d&s=%s", baseURL, runID, exp, autofixSig(secret, runID, exp))
}

func (a *App) checkAutofixLink(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("run"), 10, 64)
	if err != nil {
		return 0, false
	}
	exp, err := strconv.ParseInt(r.FormValue("e"), 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return 0, false
	}
	if !hmac.Equal([]byte(autofixSig(a.Secret, id, exp)), []byte(r.FormValue("s"))) {
		return 0, false
	}
	return id, true
}

type autofixLinkPage struct {
	E, S, Title, State string
	Done, Invalid      bool
	IssueURL           string
}

func (a *App) autofixLinkRun(w http.ResponseWriter, r *http.Request) (db.AutofixRun, db.Issue, bool) {
	id, ok := a.checkAutofixLink(r)
	if !ok {
		a.render(w, http.StatusBadRequest, "autofix", page{Title: "Auto-fix", Data: autofixLinkPage{Invalid: true}})
		return db.AutofixRun{}, db.Issue{}, false
	}
	q := db.New(a.Pool)
	run, err := q.GetAutofixRunByID(r.Context(), id)
	if err == nil {
		var is db.Issue
		is, err = q.GetIssue(r.Context(), db.GetIssueParams{ID: run.IssueID, ProjectID: run.ProjectID})
		if err == nil {
			return run, is, true
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		a.render(w, http.StatusNotFound, "autofix", page{Title: "Auto-fix", Data: autofixLinkPage{Invalid: true}})
	} else {
		a.fail(w, err)
	}
	return db.AutofixRun{}, db.Issue{}, false
}

// autofixLinkConfirm shows a confirm button: email scanners open links
// automatically, so a GET never approves anything.
func (a *App) autofixLinkConfirm(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w)
	w.Header().Set("Referrer-Policy", "no-referrer")
	run, is, ok := a.autofixLinkRun(w, r)
	if !ok {
		return
	}
	a.render(w, 200, "autofix", page{Title: "Auto-fix", Data: autofixLinkPage{
		E: r.FormValue("e"), S: r.FormValue("s"), Title: is.Title, State: run.State,
		IssueURL: fmt.Sprintf("/projects/%d/issues/%d", run.ProjectID, run.IssueID),
	}})
}

func (a *App) autofixLinkSubmit(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w)
	w.Header().Set("Referrer-Policy", "no-referrer")
	run, is, ok := a.autofixLinkRun(w, r)
	if !ok {
		return
	}
	d := autofixLinkPage{Title: is.Title, State: run.State, IssueURL: fmt.Sprintf("/projects/%d/issues/%d", run.ProjectID, run.IssueID)}
	approved, err := autofix.Approve(r.Context(), a.Pool, run.ProjectID, run.ID, 0)
	switch {
	case errors.Is(err, autofix.ErrNotAllowed):
		// Already approved, rejected or expired (the page says which), or
		// auto-fix was turned off since.
		if cur, err := db.New(a.Pool).GetAutofixRunByID(r.Context(), run.ID); err == nil {
			d.State = cur.State
		}
		if d.State == autofix.PendingApproval {
			d.State = "off"
		}
	case err != nil:
		a.fail(w, err)
		return
	default:
		d.Done, d.State = true, approved.State
		a.Log.Info("auto-fix approved by email", "project_id", run.ProjectID, "run_id", run.ID)
	}
	a.render(w, 200, "autofix", page{Title: "Auto-fix", Data: d})
}

// autofixProject loads the project for an auto-fix change. Auto-fix runs
// code on the founder's computer, so only the project's own account may
// start, approve or switch it: staff looking at another account can't.
func (a *App) autofixProject(w http.ResponseWriter, r *http.Request) (db.GetOrgProjectRow, bool) {
	p, ok := a.project(w, r)
	if !ok {
		return p, false
	}
	if sessionFrom(r).StaffView != "" {
		http.Error(w, "Only the app's own account can change auto-fix.", http.StatusForbidden)
		return p, false
	}
	return p, true
}

func (a *App) setAutofix(w http.ResponseWriter, r *http.Request) {
	p, ok := a.autofixProject(w, r)
	if !ok {
		return
	}
	rule := autofix.RuleOff
	if r.PostFormValue("rule") == autofix.RuleHigh {
		rule = autofix.RuleHigh
	}
	if err := db.New(a.Pool).SetProjectAutofix(r.Context(), db.SetProjectAutofixParams{ID: p.ID, Enabled: r.PostFormValue("enabled") == "on", Rule: rule}); err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, "/projects/"+strconv.FormatInt(p.ID, 10)+"/setup#autofix", http.StatusSeeOther)
}

func (a *App) backToIssue(w http.ResponseWriter, r *http.Request, projectID, issueID int64) {
	http.Redirect(w, r, fmt.Sprintf("/projects/%d/issues/%d#autofix", projectID, issueID), http.StatusSeeOther)
}

// startAutofix is "Auto-fix this" on an issue page: approve the waiting
// run, or start one.
func (a *App) startAutofix(w http.ResponseWriter, r *http.Request) {
	p, ok := a.autofixProject(w, r)
	if !ok {
		return
	}
	is, ok := a.issue(w, r, p.ID)
	if !ok {
		return
	}
	set, err := db.New(a.Pool).ProjectAutofix(r.Context(), p.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	if !set.AutofixEnabled {
		http.Error(w, "Turn auto-fix on in Setup first.", http.StatusConflict)
		return
	}
	if _, err := autofix.Start(r.Context(), a.Pool, p.ID, is.ID, sessionFrom(r).UserID, time.Now()); err != nil && !errors.Is(err, autofix.ErrNotAllowed) {
		a.fail(w, err)
		return
	}
	a.backToIssue(w, r, p.ID, is.ID)
}

func (a *App) autofixRunAction(w http.ResponseWriter, r *http.Request, act func(ctx context.Context, projectID, runID, userID int64) error) {
	p, ok := a.autofixProject(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("run"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	run, err := db.New(a.Pool).GetAutofixRun(r.Context(), db.GetAutofixRunParams{ID: id, ProjectID: p.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	if err := act(r.Context(), p.ID, run.ID, sessionFrom(r).UserID); err != nil && !errors.Is(err, autofix.ErrNotAllowed) {
		a.fail(w, err)
		return
	}
	a.backToIssue(w, r, p.ID, run.IssueID)
}

func (a *App) rejectAutofix(w http.ResponseWriter, r *http.Request) {
	a.autofixRunAction(w, r, func(ctx context.Context, projectID, runID, _ int64) error {
		return autofix.Reject(ctx, a.Pool, projectID, runID)
	})
}

func (a *App) approveAutofixMerge(w http.ResponseWriter, r *http.Request) {
	a.autofixRunAction(w, r, func(ctx context.Context, projectID, runID, userID int64) error {
		return autofix.ApproveMerge(ctx, a.Pool, projectID, runID, userID)
	})
}

// DaemonView is one daemon on the Setup page.
type DaemonView struct {
	db.ProjectDaemonsRow
	Online bool
}

// autofixSetup is the Setup page's auto-fix section.
func (a *App) autofixSetup(ctx context.Context, projectID int64) (map[string]any, error) {
	q := db.New(a.Pool)
	set, err := q.ProjectAutofix(ctx, projectID)
	if err != nil {
		return nil, err
	}
	rows, err := q.ProjectDaemons(ctx, projectID)
	if err != nil {
		return nil, err
	}
	daemons := make([]DaemonView, 0, len(rows))
	online := false
	for _, d := range rows {
		on := time.Since(d.LastSeenAt.Time) < autofix.OfflineAfter
		online = online || on
		daemons = append(daemons, DaemonView{ProjectDaemonsRow: d, Online: on})
	}
	runs, err := q.ListAutofixRuns(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"Enabled": set.AutofixEnabled, "Rule": set.AutofixRule, "Daemons": daemons, "Online": online,
		"Runs": runs, "DailyCap": autofix.DailyCap}, nil
}

// autofixIssue is the issue page's auto-fix block.
func (a *App) autofixIssue(ctx context.Context, projectID, issueID int64) (map[string]any, error) {
	q := db.New(a.Pool)
	set, err := q.ProjectAutofix(ctx, projectID)
	if err != nil {
		return nil, err
	}
	online, err := q.DaemonOnline(ctx, db.DaemonOnlineParams{ProjectID: projectID, Since: pgtype.Timestamptz{Time: time.Now().Add(-autofix.OfflineAfter), Valid: true}})
	if err != nil {
		return nil, err
	}
	d := map[string]any{"Enabled": set.AutofixEnabled, "Online": online}
	run, err := q.LatestAutofixRun(ctx, issueID)
	switch {
	case err == nil:
		d["Run"] = run
		d["Open"] = autofix.Open(run.State)
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}
	return d, nil
}

func autofixStateName(s string) string {
	switch s {
	case autofix.PendingApproval:
		return "Waiting for your approval"
	case autofix.Approved:
		return "Approved, waiting for the daemon"
	case autofix.Claimed:
		return "Your agent is fixing it"
	case autofix.PROpened:
		return "Pull request open"
	case autofix.AwaitingMergeApproval:
		return "Pull request waiting for your approval"
	case autofix.MergeApproved:
		return "Merge approved"
	case autofix.Merged:
		return "Merged"
	case autofix.Failed:
		return "Failed"
	case autofix.Rejected:
		return "Cancelled"
	case autofix.Expired:
		return "Expired"
	case autofix.Emailed:
		return "Daemon offline: emailed the fix"
	}
	return s
}

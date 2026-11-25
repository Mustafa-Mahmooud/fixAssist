package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yourname/fixassist/internal/ai"
	"github.com/yourname/fixassist/internal/store/db"
	"github.com/yourname/fixassist/internal/verify"
)

// FeedbackLinkTTL is how long the "Worked / Didn't work" links in alert
// emails stay valid.
const FeedbackLinkTTL = 30 * 24 * time.Hour

func feedbackSig(secret []byte, suggestionID int64, outcome string, exp int64) string {
	m := hmac.New(sha256.New, secret)
	fmt.Fprintf(m, "feedback|%d|%s|%d", suggestionID, outcome, exp)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// FeedbackLink builds a signed one-click link for an alert email.
func FeedbackLink(baseURL string, secret []byte, suggestionID int64, outcome string, now time.Time) string {
	exp := now.Add(FeedbackLinkTTL).Unix()
	return fmt.Sprintf("%s/f/%d/%s?e=%d&s=%s", baseURL, suggestionID, outcome, exp, feedbackSig(secret, suggestionID, outcome, exp))
}

func (a *App) checkFeedbackLink(r *http.Request) (int64, string, bool) {
	id, err := strconv.ParseInt(r.PathValue("sid"), 10, 64)
	if err != nil {
		return 0, "", false
	}
	outcome := r.PathValue("outcome")
	if outcome != verify.Worked && outcome != verify.Failed {
		return 0, "", false
	}
	exp, err := strconv.ParseInt(r.FormValue("e"), 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return 0, "", false
	}
	want := feedbackSig(a.Secret, id, outcome, exp)
	if !hmac.Equal([]byte(want), []byte(r.FormValue("s"))) {
		return 0, "", false
	}
	return id, outcome, true
}

type feedbackPage struct {
	Outcome, E, S, Title string
	Done, Invalid        bool
	IssueURL             string
}

// feedbackLinkPage shows a confirm button: email security scanners open
// links automatically, so a GET must never record an answer.
func (a *App) feedbackLinkPage(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w)
	w.Header().Set("Referrer-Policy", "no-referrer")
	id, outcome, ok := a.checkFeedbackLink(r)
	if !ok {
		a.render(w, http.StatusBadRequest, "feedback", page{Title: "Did it work?", Data: feedbackPage{Invalid: true}})
		return
	}
	s, err := db.New(a.Pool).GetSuggestion(r.Context(), id)
	if err != nil {
		a.render(w, http.StatusNotFound, "feedback", page{Title: "Did it work?", Data: feedbackPage{Invalid: true}})
		return
	}
	a.render(w, 200, "feedback", page{Title: "Did it work?", Data: feedbackPage{
		Outcome: outcome, E: r.FormValue("e"), S: r.FormValue("s"), Title: s.Title,
	}})
}

func (a *App) feedbackLinkSubmit(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w)
	id, outcome, ok := a.checkFeedbackLink(r)
	if !ok {
		a.render(w, http.StatusBadRequest, "feedback", page{Title: "Did it work?", Data: feedbackPage{Invalid: true}})
		return
	}
	s, err := db.New(a.Pool).GetSuggestion(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		a.render(w, http.StatusNotFound, "feedback", page{Title: "Did it work?", Data: feedbackPage{Invalid: true}})
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	if err := a.answer(r, s.ProjectID, s.IssueID, outcome, 0); err != nil {
		a.fail(w, err)
		return
	}
	a.render(w, 200, "feedback", page{Title: "Thanks", Data: feedbackPage{
		Outcome: outcome, Done: true, Title: s.Title,
		IssueURL: fmt.Sprintf("/projects/%d/issues/%d", s.ProjectID, s.IssueID),
	}})
}

// answer records an explicit answer; "didn't work" immediately writes a
// second-attempt prompt that says the first fix failed (FDR rule 3).
func (a *App) answer(r *http.Request, projectID, issueID int64, outcome string, userID int64) error {
	if err := verify.Answer(r.Context(), a.Pool, projectID, issueID, outcome, userID, time.Now()); err != nil {
		return err
	}
	if outcome == verify.Failed {
		if _, _, err := ai.AnalyzeWith(r.Context(), a.Pool, issueID, ai.AttemptAfterFailure, a.Code); err != nil {
			return err
		}
	}
	return nil
}

// VerifyProgress is shown while an issue is being verified.
type VerifyProgress struct {
	Release        string // "" = waiting for the next release
	Sessions       int64
	NeedSessions   int64
	Hours          int
	NeedHours      int
	ProbablyFixed  bool
	SessionsPct    int
	HoursPct       int
	LowTrafficDays int
}

func pct(n, of int64) int {
	if of <= 0 || n >= of {
		return 100
	}
	return int(n * 100 / of)
}

func (a *App) verifyProgress(r *http.Request, projectID int64, is db.Issue) VerifyProgress {
	rules := verify.DefaultRules
	v := VerifyProgress{
		NeedSessions: rules.MinSessions, NeedHours: int(rules.MinAge.Hours()),
		ProbablyFixed: is.ProbablyFixedAt.Valid, LowTrafficDays: int(rules.LowTrafficAfter.Hours() / 24),
	}
	if !is.VerifyReleaseID.Valid {
		return v
	}
	v.Release = is.VerifyReleaseID.String
	if n, err := db.New(a.Pool).SessionsOnRoute(r.Context(), db.SessionsOnRouteParams{ProjectID: projectID, Route: is.Route.String, ReleaseID: is.VerifyReleaseID}); err == nil {
		v.Sessions = n
	}
	if is.WatchSince.Valid {
		v.Hours = int(time.Since(is.WatchSince.Time).Hours())
	}
	v.SessionsPct, v.HoursPct = pct(v.Sessions, v.NeedSessions), pct(int64(v.Hours), int64(v.NeedHours))
	return v
}

func (a *App) issueFeedback(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok {
		return
	}
	is, ok := a.issue(w, r, p.ID)
	if !ok {
		return
	}
	outcome := r.PostFormValue("outcome")
	if outcome != verify.Worked && outcome != verify.Failed && outcome != verify.Partial {
		http.Error(w, "bad outcome", http.StatusBadRequest)
		return
	}
	if err := a.answer(r, p.ID, is.ID, outcome, sessionFrom(r).UserID); err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/projects/%d/issues/%d", p.ID, is.ID), http.StatusSeeOther)
}

func (a *App) issueApplied(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok {
		return
	}
	is, ok := a.issue(w, r, p.ID)
	if !ok {
		return
	}
	if err := verify.Applied(r.Context(), a.Pool, p.ID, is.ID, time.Now()); err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/projects/%d/issues/%d", p.ID, is.ID), http.StatusSeeOther)
}

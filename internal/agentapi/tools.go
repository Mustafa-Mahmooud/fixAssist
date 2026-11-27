package agentapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yourname/fixassist/internal/ai"
	"github.com/yourname/fixassist/internal/store/db"
	"github.com/yourname/fixassist/internal/verify"
)

// The four operations behind both the MCP tools and the JSON API. Each
// returns readable text for the agent and the same data as a struct.

// errNotFound hides other projects' issues: same answer as a missing one.
var errNotFound = errors.New("no such issue in this project")

// userError is shown to the agent as a tool error (not a server failure).
type userError struct{ msg string }

func (e userError) Error() string { return e.msg }

func kindName(k string) string {
	switch k {
	case "net":
		return "Failed request"
	case "js":
		return "Page error"
	case "server":
		return "Server error"
	}
	return "Message"
}

func when(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// IssueRow is one line of list_issues.
type IssueRow struct {
	ID          int64     `json:"id"`
	Kind        string    `json:"kind"`
	Title       string    `json:"title"`
	Route       string    `json:"route"`
	Status      string    `json:"status"`
	Events      int64     `json:"events"`
	Users       int64     `json:"users"`
	LastSeen    time.Time `json:"last_seen"`
	Regressions int32     `json:"came_back"`
}

func (h *Handler) listIssues(ctx context.Context, c caller, includeFixed bool) (string, []IssueRow, error) {
	rows, err := db.New(h.Pool).AgentIssues(ctx, db.AgentIssuesParams{ProjectID: c.ProjectID, IncludeAll: includeFixed})
	if err != nil {
		return "", nil, err
	}
	out := make([]IssueRow, 0, len(rows))
	var b strings.Builder
	fmt.Fprintf(&b, "Issues in %s (most important first):\n", c.Project)
	for _, r := range rows {
		out = append(out, IssueRow{ID: r.ID, Kind: r.Kind, Title: r.Title, Route: r.Route.String, Status: r.Status,
			Events: r.EventCount, Users: r.UserCount, LastSeen: r.LastSeenAt.Time, Regressions: r.RegressionCount})
		route := r.Route.String
		if route == "" {
			route = "background"
		}
		fmt.Fprintf(&b, "\n- #%d [%s] %s on %s, %d times, %d people, last %s: %s", r.ID, r.Status, kindName(r.Kind), route, r.EventCount, r.UserCount, when(r.LastSeenAt.Time), r.Title)
	}
	if len(rows) == 0 {
		b.WriteString("\nNo open issues. Nothing to fix right now.")
	} else {
		b.WriteString("\n\nPick one (\"new\", \"explained\" and \"open\" need a fix; \"open\" means a fix came back) and call get_issue with its id.")
	}
	return b.String(), out, nil
}

// AttemptView is one applied fix.
type AttemptView struct {
	Attempt    int       `json:"attempt"`
	Source     string    `json:"source"`
	Summary    string    `json:"summary"`
	Files      []string  `json:"files"`
	Release    string    `json:"release,omitempty"`
	ReportedAt time.Time `json:"reported_at"`
	Outcome    string    `json:"outcome"` // pending, worked, failed
}

// IssueView is get_issue's answer.
type IssueView struct {
	ID             int64         `json:"id"`
	Kind           string        `json:"kind"`
	Title          string        `json:"title"`
	Route          string        `json:"route"`
	Status         string        `json:"status"`
	Events         int64         `json:"events"`
	Users          int64         `json:"users"`
	FirstSeen      time.Time     `json:"first_seen"`
	LastSeen       time.Time     `json:"last_seen"`
	Attempt        int           `json:"attempt"` // the attempt this prompt is for
	FailedAttempts int           `json:"failed_attempts"`
	MaxAttempts    int           `json:"max_attempts"`
	WhatBroke      string        `json:"what_broke"`
	Who            string        `json:"who_is_affected"`
	Why            string        `json:"why,omitempty"`
	Prompt         string        `json:"fix_prompt"`
	HowToCheck     string        `json:"how_to_check"`
	Recent         []string      `json:"recent_occurrences"`
	Attempts       []AttemptView `json:"attempts"`
	Next           string        `json:"next_step"`
}

func (h *Handler) issue(ctx context.Context, c caller, id int64) (db.Issue, error) {
	is, err := db.New(h.Pool).GetIssue(ctx, db.GetIssueParams{ID: id, ProjectID: c.ProjectID})
	if errors.Is(err, pgx.ErrNoRows) {
		return is, errNotFound
	}
	return is, err
}

func attemptViews(list []db.FixAttempt) ([]AttemptView, int) {
	out := make([]AttemptView, 0, len(list))
	failed := 0
	for _, a := range list {
		o := a.Outcome.String
		if o == "" {
			o = "pending"
		}
		if o == verify.Failed {
			failed++
		}
		out = append(out, AttemptView{Attempt: int(a.Attempt), Source: a.Source, Summary: a.Summary, Files: a.Files,
			Release: a.ReleaseID.String, ReportedAt: a.ReportedAt.Time, Outcome: o})
	}
	return out, failed
}

func (h *Handler) getIssue(ctx context.Context, c caller, id int64) (string, IssueView, error) {
	is, err := h.issue(ctx, c, id)
	if err != nil {
		return "", IssueView{}, err
	}
	q := db.New(h.Pool)
	list, err := q.FixAttempts(ctx, is.ID)
	if err != nil {
		return "", IssueView{}, err
	}
	attempts, failed := attemptViews(list)
	// The prompt must be for the current attempt: write it now if the issue
	// hasn't been explained yet or a fix failed since the last one.
	want := max(int(is.RegressionCount), failed) + 1
	sug, err := q.LatestSuggestion(ctx, is.ID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && int(sug.Attempt) < want) {
		if _, _, err := (&ai.Analyzer{Code: h.Code, Model: h.Model, Log: h.Log}).Analyze(ctx, h.Pool, is.ID, ai.AttemptAuto); err != nil {
			return "", IssueView{}, err
		}
		sug, err = q.LatestSuggestion(ctx, is.ID)
	}
	if err != nil {
		return "", IssueView{}, err
	}
	if is, err = h.issue(ctx, c, id); err != nil { // status may have changed
		return "", IssueView{}, err
	}
	v := IssueView{
		ID: is.ID, Kind: is.Kind, Title: is.Title, Route: is.Route.String, Status: is.Status,
		Events: is.EventCount, Users: is.UserCount, FirstSeen: is.FirstSeenAt.Time, LastSeen: is.LastSeenAt.Time,
		Attempt: int(sug.Attempt), FailedAttempts: failed, MaxAttempts: verify.MaxAttempts,
		WhatBroke: sug.WhatBroke, Who: sug.WhoIsAffected, Why: sug.Why, HowToCheck: sug.HowToCheck,
		// Agents that connect here edit code, so they get the codebase framing.
		Prompt: ai.PromptForRow("cursor", sug), Attempts: attempts, Recent: []string{},
	}
	if samples, err := q.IssueSamples(ctx, db.IssueSamplesParams{IssueID: is.ID, Limit: 3}); err == nil {
		for _, s := range samples {
			line := when(s.ReceivedAt.Time)
			if s.ReleaseID.Valid {
				line += ", version " + s.ReleaseID.String
			}
			v.Recent = append(v.Recent, line)
		}
	}
	pending := len(list) > 0 && !list[len(list)-1].Outcome.Valid
	switch {
	case is.Status == "fixed":
		v.Next = "This issue is verified fixed. Nothing to do."
	case is.Status == "ignored":
		v.Next = "The founder chose to ignore this issue. Don't change anything for it."
	case failed >= verify.MaxAttempts:
		v.Next = fmt.Sprintf("%d fixes didn't hold. Don't try again: tell the founder this issue needs a person.", failed)
	case pending && is.Status == "verifying":
		v.Next = fmt.Sprintf("Attempt %d was already reported. Call check_fix(issue_id=%d) to see whether it held.", list[len(list)-1].Attempt, is.ID)
	default:
		v.Next = fmt.Sprintf("Make the one change the fix prompt asks for. Deploy or publish it (ask the user if you can't). Then call report_fix(issue_id=%d, summary=what you changed and why, files=[changed files]). Attempt %d of %d.", is.ID, v.Attempt, verify.MaxAttempts)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Issue #%d: %s\n\n", is.ID, is.Title)
	fmt.Fprintf(&b, "%s · status %s · %s · %d times, %d people · first %s, last %s\n", kindName(is.Kind), is.Status, orBackground(is.Route.String), is.EventCount, is.UserCount, when(is.FirstSeenAt.Time), when(is.LastSeenAt.Time))
	fmt.Fprintf(&b, "\n## What broke\n%s\n\n## Who is affected\n%s\n", v.WhatBroke, v.Who)
	if v.Why != "" {
		fmt.Fprintf(&b, "\n## Why\n%s\n", v.Why)
	}
	fmt.Fprintf(&b, "\n## Fix prompt (attempt %d of %d)\n%s\n", v.Attempt, verify.MaxAttempts, v.Prompt)
	if len(v.Recent) > 0 {
		fmt.Fprintf(&b, "\n## Latest occurrences\n- %s\n", strings.Join(v.Recent, "\n- "))
	}
	if len(attempts) > 0 {
		b.WriteString("\n## Fixes applied so far\n")
		for _, a := range attempts {
			fmt.Fprintf(&b, "- Attempt %d (%s, %s): %s", a.Attempt, a.Source, when(a.ReportedAt), a.Outcome)
			if a.Summary != "" {
				fmt.Fprintf(&b, ": %s", a.Summary)
			}
			b.WriteString("\n")
		}
	}
	fmt.Fprintf(&b, "\n## Next step\n%s", v.Next)
	return b.String(), v, nil
}

func orBackground(route string) string {
	if route == "" {
		return "background job"
	}
	return route
}

// ReportArgs is report_fix's input.
type ReportArgs struct {
	IssueID int64    `json:"issue_id"`
	Summary string   `json:"summary"`
	Files   []string `json:"files"`
	Release string   `json:"release"`
}

// ReportView is report_fix's answer.
type ReportView struct {
	Attempt int    `json:"attempt"`
	Next    string `json:"next_step"`
}

func (h *Handler) reportFix(ctx context.Context, c caller, a ReportArgs, now time.Time) (string, ReportView, error) {
	is, err := h.issue(ctx, c, a.IssueID)
	if err != nil {
		return "", ReportView{}, err
	}
	a.Summary = strings.TrimSpace(a.Summary)
	switch {
	case a.Summary == "":
		return "", ReportView{}, userError{"summary is required: say in a sentence or two what you changed and why."}
	case len(a.Summary) > 2000:
		return "", ReportView{}, userError{"summary is too long (2000 characters at most)."}
	case len(a.Files) > 50:
		return "", ReportView{}, userError{"list at most 50 files."}
	case len(a.Release) > 200:
		return "", ReportView{}, userError{"release is too long."}
	}
	files := make([]string, 0, len(a.Files))
	for _, f := range a.Files {
		if f = strings.TrimSpace(f); f != "" && len(f) <= 300 {
			files = append(files, f)
		}
	}
	n, err := verify.ReportFix(ctx, h.Pool, c.ProjectID, is.ID, verify.Report{Source: "agent", TokenID: c.TokenID, Release: a.Release, Summary: a.Summary, Files: files}, now)
	switch {
	case errors.Is(err, verify.ErrTooManyAttempts), errors.Is(err, verify.ErrNotFixable):
		return "", ReportView{}, userError{err.Error()}
	case err != nil:
		return "", ReportView{}, err
	}
	h.Log.Info("agent reported a fix", "project_id", c.ProjectID, "issue_id", is.ID, "attempt", n, "token_id", c.TokenID)
	v := ReportView{Attempt: n, Next: fmt.Sprintf("Recorded as attempt %d. Fix Assistant is now watching for the error on the fixed version. Call check_fix(issue_id=%d); if it asks, repeat the failing action and call check_fix(issue_id=%d, reproduced=true).", n, is.ID, is.ID)}
	return v.Next, v, nil
}

// CheckView is check_fix's answer.
type CheckView struct {
	State       string    `json:"state"`
	Attempt     int       `json:"attempt"`
	Failed      int       `json:"failed_attempts"`
	Release     string    `json:"release,omitempty"`
	Since       time.Time `json:"watching_since,omitzero"`
	Minutes     int       `json:"minutes"`
	Sessions    int64     `json:"sessions_on_page"`
	Occurrences int64     `json:"occurrences_since_fix"`
	LastSeen    time.Time `json:"last_seen,omitzero"`
	Next        string    `json:"next_step"`
}

func (h *Handler) checkFix(ctx context.Context, c caller, id int64, reproduced bool, now time.Time) (string, CheckView, error) {
	is, err := h.issue(ctx, c, id)
	if err != nil {
		return "", CheckView{}, err
	}
	r, err := verify.QuickCheck(ctx, h.Pool, is, reproduced, now)
	if err != nil {
		return "", CheckView{}, err
	}
	v := CheckView{State: r.State, Attempt: r.Attempt, Failed: r.Failed, Release: r.Release, Since: r.Since, Minutes: r.Minutes,
		Sessions: r.Sessions, Occurrences: r.Occurrences, LastSeen: r.LastSeen, Next: r.Message}
	var b strings.Builder
	fmt.Fprintf(&b, "State: %s", r.State)
	if r.Attempt > 0 {
		fmt.Fprintf(&b, " (attempt %d of %d)", r.Attempt, verify.MaxAttempts)
	}
	if !r.Since.IsZero() {
		fmt.Fprintf(&b, "\nWatching since %s", when(r.Since))
		if r.Release != "" {
			fmt.Fprintf(&b, " on version %s", r.Release)
		}
		fmt.Fprintf(&b, ": %d occurrence(s), %d session(s) on the page, %d min.", r.Occurrences, r.Sessions, r.Minutes)
	}
	fmt.Fprintf(&b, "\n\n%s", r.Message)
	return b.String(), v, nil
}

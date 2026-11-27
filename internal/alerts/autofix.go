package alerts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yourname/fixassist/internal/store/db"
)

// MaxAutofixEmailsPerDay caps auto-fix approval emails per project; the rest
// wait on the issue page.
const MaxAutofixEmailsPerDay = 10

func (n *Notifier) issueLink(projectID, issueID int64) string {
	return fmt.Sprintf("%s/projects/%d/issues/%d", strings.TrimRight(n.BaseURL, "/"), projectID, issueID)
}

func (n *Notifier) autofixRun(ctx context.Context, q *db.Queries, r db.AutofixRun) (db.GetIssueForAnalysisRow, db.FixSuggestion, error) {
	is, err := q.GetIssueForAnalysis(ctx, r.IssueID)
	if err != nil {
		return is, db.FixSuggestion{}, err
	}
	var s db.FixSuggestion
	if r.SuggestionID.Valid {
		s, err = q.SuggestionByID(ctx, r.SuggestionID.Int64)
	} else {
		s, err = q.LatestSuggestion(ctx, r.IssueID)
	}
	return is, s, err
}

// AutofixApproval asks the founder to approve an auto-fix run. The link
// opens a confirm page; nothing is approved by opening it.
func (n *Notifier) AutofixApproval(ctx context.Context, r db.AutofixRun) error {
	q := db.New(n.Pool)
	sent, err := q.CountAutofixAlertsSince(ctx, db.CountAutofixAlertsSinceParams{ProjectID: r.ProjectID, Since: pgtype.Timestamptz{Time: n.now().Add(-24 * time.Hour), Valid: true}})
	if err != nil {
		return err
	}
	if sent >= MaxAutofixEmailsPerDay {
		return nil
	}
	is, s, err := n.autofixRun(ctx, q, r)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	link := n.issueLink(is.ProjectID, is.ID)
	approve := link
	if n.AutofixLink != nil {
		approve = n.AutofixLink(r.ID)
	}
	subject := fmt.Sprintf("[%s] Auto-fix? %s", is.ProjectName, s.Title)
	body := fmt.Sprintf("%s\n\n%s\n\nYour coding agent can fix this on its own: it works in a separate branch, opens a pull request, and merges it once your checks pass.\n\nApprove: %s\nDetails: %s\n", s.WhatBroke, s.WhoIsAffected, approve, link)
	html := RenderHTML(Email{
		Preheader: "Approve and your agent fixes it: " + s.WhatBroke,
		Label:     "Auto-fix",
		Tone:      "bad",
		Heading:   s.Title,
		Paras: []string{s.WhatBroke, s.WhoIsAffected,
			"Your coding agent can fix this on its own: it works in a separate branch, opens a pull request, and merges it once your checks pass."},
		Buttons: []Button{{"Auto-fix this", approve}, {"Open the issue", link}},
		Note:    "Nothing happens until you confirm on the next page.",
	})
	id, created, err := insertAlert(ctx, q, r.ProjectID, pgtype.Int8{Int64: r.IssueID, Valid: true}, "autofix", n.now().UTC(), subject, body)
	if err != nil || !created {
		return err
	}
	return n.deliver(ctx, q, id, r.ProjectID, "autofix", subject, body, html)
}

// AutofixOffline sends an approved run's fix prompt to paste, because no
// daemon was online to take it. Skipped when the instant alert already
// emailed the same prompt in the last day.
func (n *Notifier) AutofixOffline(ctx context.Context, r db.AutofixRun) error {
	q := db.New(n.Pool)
	dup, err := q.InstantAlertedSince(ctx, db.InstantAlertedSinceParams{IssueID: pgtype.Int8{Int64: r.IssueID, Valid: true}, Since: pgtype.Timestamptz{Time: n.now().Add(-24 * time.Hour), Valid: true}})
	if err != nil || dup {
		return err
	}
	is, s, err := n.autofixRun(ctx, q, r)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	link := n.issueLink(is.ProjectID, is.ID)
	prompt := n.Prompt(is.TargetAgent, s)
	subject := fmt.Sprintf("[%s] Fix by hand: %s", is.ProjectName, s.Title)
	body := fmt.Sprintf("You approved an auto-fix, but the Fix Assistant daemon on your computer hasn't been online for 10 minutes, so here is the fix to paste into %s instead:\n\n----------\n%s\n----------\n\nDetails: %s\n", agentName(is.TargetAgent), prompt, link)
	html := RenderHTML(Email{
		Preheader: "Your auto-fix daemon is offline: paste this fix instead.",
		Label:     "Auto-fix",
		Tone:      "warn",
		Heading:   s.Title,
		Paras: []string{"You approved an auto-fix, but the Fix Assistant daemon on your computer hasn't been online for 10 minutes. Start it with `fixassist watch`, or paste this fix yourself.",
			s.WhatBroke},
		CodeLabel: "Paste this into " + agentName(is.TargetAgent) + ":",
		Code:      prompt,
		Buttons:   []Button{{"Open the issue", link}},
	})
	id, created, err := insertAlert(ctx, q, r.ProjectID, pgtype.Int8{Int64: r.IssueID, Valid: true}, "autofix", n.now().UTC(), subject, body)
	if err != nil || !created {
		return err
	}
	return n.deliver(ctx, q, id, r.ProjectID, "autofix", subject, body, html)
}

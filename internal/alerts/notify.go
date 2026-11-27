package alerts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/fixassist/internal/analytics"
	"github.com/yourname/fixassist/internal/store/db"
)

// FDR alert rules.
const (
	InstantMinImpact     = 3 // e.g. 1 person on checkout/signup/login, or 3 elsewhere
	MaxInstantPerProject = 3 // per day; the rest wait for the digest
	DigestHour           = 9 // local time
)

// PromptFor renders a stored suggestion's fix prompt in the agent's style.
// Set by cmd/app to ai.ForAgent (alerts must not import ai: ai imports grouping).
type PromptFor func(agent string, s db.FixSuggestion) string

// Notifier writes alert_log rows (shown in the dashboard) and emails them
// unless email is off.
type Notifier struct {
	Pool     *pgxpool.Pool
	Sender   Sender
	BaseURL  string
	EmailOff bool
	Prompt   PromptFor
	// FeedbackLink, if set, adds signed "Worked / Didn't work" links to
	// instant alerts (set by cmd/app to web.FeedbackLink).
	FeedbackLink func(suggestionID int64, outcome string) string
	// AutofixLink, if set, is the signed "Auto-fix this" link in auto-fix
	// approval emails (set by cmd/app to web.AutofixLink).
	AutofixLink func(runID int64) string
	Log         *slog.Logger
	Now         func() time.Time
}

func (n *Notifier) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}

func date(t time.Time) pgtype.Date { return pgtype.Date{Time: t, Valid: true} }

func agentName(a string) string {
	switch a {
	case "lovable":
		return "Lovable"
	case "bolt":
		return "Bolt"
	case "cursor":
		return "Cursor"
	case "replit":
		return "Replit"
	case "v0":
		return "v0"
	}
	return "your AI agent"
}

// InstantResult says what CheckInstant did.
type InstantResult string

const (
	SentInstant      InstantResult = "sent"
	SkipNoSuggestion InstantResult = "no_suggestion"
	SkipLowImpact    InstantResult = "low_impact"
	SkipAlreadyToday InstantResult = "already_today"
	SkipDailyCap     InstantResult = "daily_cap"
	SkipIgnored      InstantResult = "ignored"
)

// CheckInstant applies the FDR instant-alert rules to one issue.
func (n *Notifier) CheckInstant(ctx context.Context, issueID int64) (InstantResult, error) {
	q := db.New(n.Pool)
	is, err := q.GetIssueForAnalysis(ctx, issueID)
	if err != nil {
		return "", err
	}
	if is.Status == "ignored" || is.Status == "fixed" {
		return SkipIgnored, nil
	}
	s, err := q.LatestSuggestion(ctx, issueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return SkipNoSuggestion, nil // never alert without a fix prompt to paste
	}
	if err != nil {
		return "", err
	}
	impact, _ := is.ImpactScore.Float64Value()
	regressed := is.Status == "open" && is.RegressionCount > 0
	if impact.Float64 < InstantMinImpact && !regressed {
		return SkipLowImpact, nil
	}
	today := n.now().UTC()

	subject := fmt.Sprintf("[%s] %s", is.ProjectName, s.Title)
	if regressed {
		subject = fmt.Sprintf("[%s] Came back: %s", is.ProjectName, s.Title)
	}
	link := fmt.Sprintf("%s/projects/%d/issues/%d", strings.TrimRight(n.BaseURL, "/"), is.ProjectID, is.ID)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n%s\n", s.WhatBroke, s.WhoIsAffected)
	if s.Why != "" {
		fmt.Fprintf(&b, "\nWhy: %s\n", s.Why)
	}
	prompt := n.Prompt(is.TargetAgent, s)
	fmt.Fprintf(&b, "\nPaste this into %s:\n\n----------\n%s\n----------\n\nDetails: %s\n", agentName(is.TargetAgent), prompt, link)
	e := Email{
		Preheader: s.WhatBroke,
		Label:     "New issue",
		Tone:      "bad",
		Heading:   s.Title,
		Paras:     []string{s.WhatBroke, s.WhoIsAffected},
		CodeLabel: "Paste this into " + agentName(is.TargetAgent) + ":",
		Code:      prompt,
		Buttons:   []Button{{"Open the issue", link}},
	}
	if regressed {
		e.Label = "Came back"
	}
	if s.Why != "" {
		e.Paras = append(e.Paras, "Why: "+s.Why)
	}
	if n.FeedbackLink != nil {
		e.Buttons = append(e.Buttons, Button{"It worked", n.FeedbackLink(s.ID, "worked")}, Button{"Didn't work", n.FeedbackLink(s.ID, "failed")})
		e.Note = "Tried the fix? Tell us with the buttons above so we can learn what works."
	}
	html := RenderHTML(e)
	// Check the daily cap and store the alert atomically per project:
	// concurrent alert checks would otherwise both see "2 sent today" and
	// both send. Email goes out after commit.
	var id int64
	var capped, created bool
	err = pgx.BeginFunc(ctx, n.Pool, func(tx pgx.Tx) error {
		tq := db.New(tx)
		if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock($1, $2)", instantCapLock, int32(is.ProjectID)); err != nil {
			return err
		}
		sent, err := tq.CountInstantAlertsToday(ctx, db.CountInstantAlertsTodayParams{ProjectID: is.ProjectID, Day: date(today)})
		if err != nil {
			return err
		}
		if sent >= MaxInstantPerProject {
			capped = true
			return nil
		}
		id, created, err = insertAlert(ctx, tq, is.ProjectID, pgtype.Int8{Int64: is.ID, Valid: true}, "instant", today, subject, b.String())
		return err
	})
	switch {
	case err != nil:
		return "", err
	case capped:
		return SkipDailyCap, nil
	case !created:
		return SkipAlreadyToday, nil
	}
	return SentInstant, n.deliver(ctx, q, id, is.ProjectID, "instant", subject, b.String(), html)
}

// instantCapLock namespaces the per-project advisory lock for the daily cap.
const instantCapLock int32 = 726_2002

// record stores the alert (idempotent per issue/day or project/day) and
// emails it. created is false when this alert already exists for the day.
func (n *Notifier) record(ctx context.Context, q *db.Queries, projectID int64, issueID pgtype.Int8, kind string, day time.Time, subject, body, html string) (created bool, err error) {
	id, created, err := insertAlert(ctx, q, projectID, issueID, kind, day, subject, body)
	if err != nil || !created {
		return created, err
	}
	return true, n.deliver(ctx, q, id, projectID, kind, subject, body, html)
}

// insertAlert stores the alert; created is false if it already exists for
// the day (unique index), so job retries never double-email.
func insertAlert(ctx context.Context, q *db.Queries, projectID int64, issueID pgtype.Int8, kind string, day time.Time, subject, body string) (int64, bool, error) {
	id, err := q.InsertAlert(ctx, db.InsertAlertParams{
		ProjectID: projectID, IssueID: issueID, Kind: kind, Day: date(day), Subject: subject, Body: body, EmailedTo: []string{},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

// deliver emails a stored alert to the project's members (unless email is off).
func (n *Notifier) deliver(ctx context.Context, q *db.Queries, id, projectID int64, kind, subject, body, html string) error {
	if n.EmailOff {
		n.Log.Info("alert stored (email off)", "kind", kind, "project_id", projectID, "subject", subject)
		return nil
	}
	to, err := q.ProjectRecipients(ctx, projectID)
	if err != nil {
		return err
	}
	var sent []string
	for _, addr := range to {
		if err := n.Sender.Send(ctx, Message{To: addr, Subject: subject, Text: body, HTML: html}); err != nil {
			n.Log.Warn("alert email failed", "to", addr, "err", err)
			continue
		}
		sent = append(sent, addr)
	}
	return q.SetAlertEmailed(ctx, db.SetAlertEmailedParams{ID: id, EmailedTo: sent})
}

// RunDigests sends the daily digest to every project whose local time is
// DigestHour (FDR: "hourly sweep; sends at 09:00 in the founder's time zone").
// Skipped when there is nothing new.
func (n *Notifier) RunDigests(ctx context.Context) (int, error) {
	q := db.New(n.Pool)
	ps, err := q.ProjectsForDigest(ctx)
	if err != nil {
		return 0, err
	}
	sentCount := 0
	for _, p := range ps {
		loc, err := time.LoadLocation(p.Timezone)
		if err != nil {
			loc = time.UTC
		}
		local := n.now().In(loc)
		if local.Hour() != DigestHour {
			continue
		}
		since := n.now().Add(-24 * time.Hour)
		newIssues, err := q.DigestNewIssues(ctx, db.DigestNewIssuesParams{ProjectID: p.ID, FirstSeenAt: pgtype.Timestamptz{Time: since, Valid: true}})
		if err != nil {
			return sentCount, err
		}
		fixed, err := q.DigestFixedSince(ctx, db.DigestFixedSinceParams{ProjectID: p.ID, FixedAt: pgtype.Timestamptz{Time: since, Valid: true}})
		if err != nil {
			return sentCount, err
		}
		if len(newIssues) == 0 && fixed == 0 {
			continue // nothing new: no email
		}
		open, err := q.DigestOpenCount(ctx, p.ID)
		if err != nil {
			return sentCount, err
		}
		views, err := q.DigestPageviews(ctx, db.DigestPageviewsParams{ProjectID: p.ID,
			Ts: pgtype.Timestamptz{Time: since, Valid: true}, Ts_2: pgtype.Timestamptz{Time: n.now(), Valid: true}})
		if err != nil {
			return sentCount, err
		}
		base := strings.TrimRight(n.BaseURL, "/")
		var b strings.Builder
		fmt.Fprintf(&b, "Your app %s in the last 24 hours:\n\n", p.Name)
		e := Email{
			Label:   "Daily summary",
			Heading: p.Name + " in the last 24 hours",
			Stats: []Stat{
				{"New issues", strconv.Itoa(len(newIssues))},
				{"Fixed", strconv.FormatInt(fixed, 10)},
				{"Still open", strconv.FormatInt(open, 10)},
				{"Page views", strconv.FormatInt(views, 10)},
			},
			Buttons: []Button{{"Open the dashboard", fmt.Sprintf("%s/projects/%d", base, p.ID)}},
		}
		if len(newIssues) > 0 {
			fmt.Fprintf(&b, "New issues (%d):\n", len(newIssues))
			for _, i := range newIssues {
				sev := ""
				if i.Severity != "" {
					sev = " [" + i.Severity + "]"
				}
				meta := countWord(i.UserCount, "person", "people") + " affected, " + countWord(i.EventCount, "time", "times")
				url := fmt.Sprintf("%s/projects/%d/issues/%d", base, p.ID, i.ID)
				fmt.Fprintf(&b, "- %s%s: %s, %s\n  %s\n", i.Title, sev,
					countWord(i.UserCount, "person", "people"), countWord(i.EventCount, "time", "times"), url)
				e.Items = append(e.Items, Item{Title: i.Title, Meta: meta, URL: url, Badge: i.Severity})
			}
			b.WriteString("\n")
			e.Preheader = countWord(int64(len(newIssues)), "new issue", "new issues") + ", " + strconv.FormatInt(open, 10) + " still open."
		} else {
			e.Preheader = countWord(fixed, "issue", "issues") + " fixed since yesterday. Nice work."
			e.Paras = []string{"No new issues since yesterday."}
			e.Tone = "ok"
		}
		if fixed > 0 {
			fmt.Fprintf(&b, "Fixed since yesterday: %d\n", fixed)
		}
		fmt.Fprintf(&b, "Still open: %d\nPage views: %d\n", open, views)
		subject := fmt.Sprintf("[%s] Daily summary: %s", p.Name, countWord(int64(len(newIssues)), "new issue", "new issues"))
		created, err := n.record(ctx, q, p.ID, pgtype.Int8{}, "digest", local, subject, b.String(), RenderHTML(e))
		if err != nil {
			return sentCount, err
		}
		if created {
			sentCount++
		}
	}
	return sentCount, nil
}

// CheckQuotas emails each project once per month when it passes 80% and
// 100% of its monthly event quota (FDR quotas). Over 100%, ingest answers
// 429 until next month.
func (n *Notifier) CheckQuotas(ctx context.Context) (int, error) {
	q := db.New(n.Pool)
	now := n.now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	rows, err := q.QuotaUsage(ctx, date(month))
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, r := range rows {
		if r.QuotaMonthly <= 0 {
			continue
		}
		pct := r.Events * 100 / r.QuotaMonthly
		var kind, subject, body string
		reset := month.AddDate(0, 1, 0).Format("2 January")
		e := Email{
			Stats: []Stat{
				{"Events used", strconv.FormatInt(r.Events, 10)},
				{"Monthly limit", strconv.FormatInt(r.QuotaMonthly, 10)},
				{"Resets", reset},
			},
			Buttons: []Button{{"See the issues", fmt.Sprintf("%s/projects/%d", strings.TrimRight(n.BaseURL, "/"), r.ID)}},
		}
		switch {
		case pct >= 100:
			kind = "quota100"
			subject = fmt.Sprintf("[%s] Monthly event limit reached", r.Name)
			body = fmt.Sprintf("Your app %s has sent %d of its %d monthly events. New errors aren't recorded until %s.\n\nUsually this means one error is firing in a loop; the issue list shows which one.\n",
				r.Name, r.Events, r.QuotaMonthly, reset)
			e.Label, e.Tone, e.Heading = "Limit reached", "bad", r.Name+" reached its monthly event limit"
			e.Paras = []string{"New errors aren't recorded until " + reset + ".", "Usually this means one error is firing in a loop; the issue list shows which one."}
		case pct >= 80:
			kind = "quota80"
			subject = fmt.Sprintf("[%s] 80%% of this month's events used", r.Name)
			body = fmt.Sprintf("Your app %s has sent %d of its %d monthly events (%d%%). At 100%% new errors stop being recorded until %s.\n",
				r.Name, r.Events, r.QuotaMonthly, pct, reset)
			e.Label, e.Tone, e.Heading = "Usage warning", "warn", fmt.Sprintf("%s has used %d%% of this month's events", r.Name, pct)
			e.Paras = []string{"At 100% new errors stop being recorded until " + reset + "."}
		default:
			continue
		}
		e.Preheader = e.Paras[0]
		created, err := n.record(ctx, q, r.ID, pgtype.Int8{}, kind, month, subject, body, RenderHTML(e))
		if err != nil {
			return sent, err
		}
		if created {
			sent++
		}
	}
	return sent, nil
}

// RunGrowthReports makes the weekly insights (Monday 09:00 project time),
// stores a summary alert (emailed when email is on), and runs the
// before/after check on earlier insights.
func (n *Notifier) RunGrowthReports(ctx context.Context) (int, error) {
	if _, err := analytics.Evaluate(ctx, n.Pool, n.now()); err != nil {
		return 0, err
	}
	q := db.New(n.Pool)
	ps, err := q.ProjectsForDigest(ctx)
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, p := range ps {
		loc, err := time.LoadLocation(p.Timezone)
		if err != nil {
			loc = time.UTC
		}
		local := n.now().In(loc)
		if local.Weekday() != time.Monday || local.Hour() != DigestHour {
			continue
		}
		if _, err := analytics.Generate(ctx, n.Pool, p.ID, n.now()); err != nil {
			return sent, err
		}
		week := analytics.WeekStart(n.now())
		list, err := q.InsightsForWeek(ctx, db.InsightsForWeekParams{ProjectID: p.ID, Week: date(week)})
		if err != nil || len(list) == 0 {
			if err != nil {
				return sent, err
			}
			continue
		}
		insights := fmt.Sprintf("%s/projects/%d/insights", strings.TrimRight(n.BaseURL, "/"), p.ID)
		var b strings.Builder
		fmt.Fprintf(&b, "This week's ideas to get more sign-ups for %s:\n\n", p.Name)
		e := Email{
			Label:     "Weekly ideas",
			Tone:      "ok",
			Heading:   "Ideas to get more sign-ups for " + p.Name,
			Paras:     []string{"Each idea comes with a prompt you can paste into your AI agent."},
			Buttons:   []Button{{"See the prompts", insights}},
			Preheader: list[0].Title,
		}
		for i, in := range list {
			fmt.Fprintf(&b, "%d. %s\n   %s\n\n", i+1, in.Title, in.Evidence)
			e.Items = append(e.Items, Item{Title: fmt.Sprintf("%d. %s", i+1, in.Title), Meta: in.Evidence, URL: insights})
		}
		fmt.Fprintf(&b, "Each comes with a prompt for your AI agent: %s\n", insights)
		created, err := n.record(ctx, q, p.ID, pgtype.Int8{}, "growth", week, fmt.Sprintf("[%s] %s to get more sign-ups this week", p.Name, countWord(int64(len(list)), "idea", "ideas")), b.String(), RenderHTML(e))
		if err != nil {
			return sent, err
		}
		if created {
			sent++
		}
	}
	return sent, nil
}

func countWord(n int64, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.FormatInt(n, 10) + " " + many
}

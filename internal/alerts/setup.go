package alerts

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yourname/fixassist/internal/sitecheck"
	"github.com/yourname/fixassist/internal/store/db"
)

// Setup reminders bring back founders who stopped half way:
//
//   - setup_install: a day after adding an app, still no visit. Says what
//     "Check my site" finds now, and any address whose visits were refused;
//   - setup_test: connected for three days but nothing caught. Good news,
//     plus a link that sends a test error, so they see what an issue is.
//
// Each goes out at most once per app (unique index), in the founder's
// daytime, and never after they hid the setup list.

const (
	SetupInstallAfter = 24 * time.Hour
	SetupTestAfter    = 72 * time.Hour
	// Sent between these local hours, so a reminder isn't a 3 a.m. email.
	setupFromHour, setupToHour = 9, 18
)

// SetupChecker checks a founder's live site (sitecheck.Check; a fake in tests).
type SetupChecker func(ctx context.Context, s sitecheck.Site) sitecheck.Result

func (n *Notifier) checkSite(ctx context.Context, s sitecheck.Site) sitecheck.Result {
	if n.SiteCheck != nil {
		return n.SiteCheck(ctx, s)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return sitecheck.Check(ctx, nil, s)
}

// RunSetupNudges sends the setup reminders that are due.
func (n *Notifier) RunSetupNudges(ctx context.Context) (int, error) {
	q := db.New(n.Pool)
	ps, err := q.ProjectsForSetupNudge(ctx, pgtype.Timestamptz{Time: n.now(), Valid: true})
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
		if local.Hour() < setupFromHour || local.Hour() >= setupToHour {
			continue
		}
		age := n.now().Sub(p.CreatedAt.Time)
		var kind, subject, text string
		var e Email
		switch {
		case !p.Connected && !p.InstallSent && age >= SetupInstallAfter:
			kind = "setup_install"
			subject, text, e, err = n.installNudge(ctx, q, p)
			if err != nil {
				return sent, err
			}
		case p.Connected && !p.HasIssue && !p.TestSent && age >= SetupTestAfter:
			kind = "setup_test"
			subject, text, e = n.testNudge(p)
		default:
			continue
		}
		created, err := n.record(ctx, q, p.ID, pgtype.Int8{}, kind, local, subject, text, RenderHTML(e))
		if err != nil {
			return sent, err
		}
		if created {
			sent++
		}
	}
	return sent, nil
}

func (n *Notifier) setupURL(projectID int64) string {
	return fmt.Sprintf("%s/projects/%d/setup", strings.TrimRight(n.BaseURL, "/"), projectID)
}

// installNudge: no visit yet. What's wrong, as far as we can tell.
func (n *Notifier) installNudge(ctx context.Context, q *db.Queries, p db.ProjectsForSetupNudgeRow) (string, string, Email, error) {
	builder := agentName(p.TargetAgent)
	setup := n.setupURL(p.ID)
	e := Email{
		Label:     "Setup",
		Tone:      "warn",
		Heading:   p.Name + " hasn't sent anything yet",
		Preheader: "No visits from your app so far. Here's what we found.",
		Buttons:   []Button{{"Finish setup", setup}},
		Note:      "Already done? Just open your live app once: the first visit shows up within seconds. You won't get this reminder again.",
	}
	var b strings.Builder
	fmt.Fprintf(&b, "You added %s to Fix Assistant yesterday, but no visits have arrived from it yet.\n\n", p.Name)
	e.Paras = append(e.Paras, fmt.Sprintf("You added %s yesterday, but no visits have arrived from it yet. Here's what we found when we looked:", p.Name))

	refused, err := q.ListRejectedOrigins(ctx, p.ID)
	if err != nil {
		return "", "", e, err
	}
	if len(refused) > 0 {
		host := refused[0].Host
		line := fmt.Sprintf("Visits came from %s, which isn't one of this app's addresses, so they were refused. If that's your app, allow it on the Setup page.", host)
		fmt.Fprintf(&b, "%s\n\n", line)
		e.Items = append(e.Items, Item{Title: "Visits from another address were refused", Meta: line, URL: setup})
	}
	if len(p.AllowedDomains) > 0 && p.PublicKey != "" {
		c := n.checkSite(ctx, sitecheck.Site{Domains: p.AllowedDomains, PublicKey: p.PublicKey, IngestBase: n.IngestBase, Builder: builder})
		fmt.Fprintf(&b, "%s\n%s\n\n", c.Title, c.Text)
		e.Items = append(e.Items, Item{Title: c.Title, Meta: c.Text, URL: setup})
	}
	if len(e.Items) == 0 {
		line := fmt.Sprintf("Paste the install message from Setup into %s, publish your app, then open it once.", builder)
		fmt.Fprintf(&b, "%s\n\n", line)
		e.Paras = append(e.Paras, line)
	}
	fmt.Fprintf(&b, "Finish setup: %s\n", setup)
	return fmt.Sprintf("[%s] Finish setting up Fix Assistant", p.Name), b.String(), e, nil
}

// testNudge: connected, nothing caught. Show what an issue looks like.
func (n *Notifier) testNudge(p db.ProjectsForSetupNudgeRow) (string, string, Email) {
	setup := n.setupURL(p.ID)
	e := Email{
		Label:     "Good news",
		Tone:      "ok",
		Heading:   "No errors in " + p.Name + " so far",
		Preheader: "Fix Assistant is watching and hasn't caught a problem yet.",
		Paras: []string{
			fmt.Sprintf("Fix Assistant has been watching %s for a few days and hasn't caught a single error. That's a good sign.", p.Name),
			"Want to see what you'll get when something breaks? Send a test error: it opens your app and reports one harmless error, explained a few seconds later.",
		},
		Note: "You won't get this email again.",
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n%s\n\n", e.Paras[0], e.Paras[1])
	if u := sitecheck.TestURL(p.AllowedDomains); u != "" {
		e.Buttons = []Button{{"Send a test error", u}, {"Open Fix Assistant", setup}}
		fmt.Fprintf(&b, "Send a test error: %s\n", u)
	} else {
		e.Buttons = []Button{{"Open Fix Assistant", setup}}
	}
	fmt.Fprintf(&b, "Fix Assistant: %s\n", setup)
	return fmt.Sprintf("[%s] No errors so far, here's a quick test", p.Name), b.String(), e
}

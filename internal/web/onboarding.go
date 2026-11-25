package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yourname/fixassist/internal/store/db"
)

// Onboarding for first-time founders:
//
//   - /projects shows a welcome page (what Fix Assistant does, how the loop
//     works) with the first app's form, until the first app exists;
//   - every project page shows a "Getting started" checklist, ticked from the
//     data itself (first page view, first issue, ...), until dismissed;
//   - every project tab starts with a short "what this page shows" tip,
//     until the person hides tips.

// Step is one item of the checklist.
type Step struct {
	Title, Text, Code string
	Link, LinkText    string
	Done, Optional    bool
}

// Onboarding is a project's checklist.
type Onboarding struct {
	ProjectID     int64
	Steps         []Step
	Done, Of      int // core steps done, of
	CoreDone      bool
	ShowOptionals bool
}

// testErrorJS is what a founder runs in the browser console to see an issue.
const testErrorJS = `setTimeout(() => { throw new Error("Fix Assistant test") })`

func (a *App) onboarding(ctx context.Context, p db.GetOrgProjectRow) *Onboarding {
	st, err := db.New(a.Pool).ProjectOnboarding(ctx, p.ID)
	if err != nil {
		a.Log.Warn("onboarding progress", "project_id", p.ID, "err", err)
		return nil
	}
	if st.Dismissed {
		return nil
	}
	base := "/projects/" + strconv.FormatInt(p.ID, 10)
	issueLink, issueText := base+"/issues", "Open Issues"
	if st.FirstIssueID != 0 {
		issueLink, issueText = fmt.Sprintf("%s/issues/%d", base, st.FirstIssueID), "Open the issue"
	}
	o := &Onboarding{ProjectID: p.ID, Steps: []Step{
		{Title: "Add your app", Text: "Done. Fix Assistant knows which builder and backend you use, so its explanations and fix messages fit your app.", Done: true},
		{Title: "Install the snippet", Done: st.Connected, Link: base + "/setup", LinkText: "Go to Setup",
			Text: fmt.Sprintf("Copy the install message from Setup, paste it into %s, publish your app, then open your live site once. This step ticks itself when the first visit arrives.", agentName(p.TargetAgent))},
		{Title: "See your first issue", Done: st.HasIssue, Link: base + "/issues", LinkText: "Open Issues",
			Text: "Problems show up here on their own as visitors use your app. To see one right away, open your live site, press F12, choose Console, and run:",
			Code: testErrorJS},
		{Title: "Read the explanation and copy the fix", Done: st.Explained, Link: issueLink, LinkText: issueText,
			Text: "An issue says what broke, who is affected and why, with a fix message to paste into your AI builder. Issues are explained once 2 people or 3 visits hit them; click Explain now to see it sooner. After publishing the fix, click I applied the fix and Fix Assistant checks that the error stopped."},
		{Title: "Catch errors from your server too", Optional: true, Done: st.ServerErrors, Link: base + "/setup#server-errors", LinkText: "Set up server errors",
			Text: "If your app has its own backend (Node.js, Python, .NET, PHP, ...), errors there, like a failed save to the database, need a small package on the server."},
	}}
	if a.GitHub != nil {
		o.Steps = append(o.Steps, Step{Title: "Connect your code", Optional: true, Done: st.CodeConnected, Link: base + "/setup", LinkText: "Connect GitHub",
			Text: "With your GitHub repository connected (read-only), fix messages point to the exact file and line."})
	}
	o.Steps = append(o.Steps, Step{Title: "Let your AI agent fix issues by itself", Optional: true, Done: st.AgentConnected, Link: base + "/setup#agent-loop", LinkText: "Connect an agent",
		Text: "Coding agents like Cursor or Claude Code can connect to Fix Assistant, fix an issue, and keep trying until the error has really stopped."})
	for _, s := range o.Steps {
		if !s.Optional {
			o.Of++
			if s.Done {
				o.Done++
			}
		}
	}
	o.CoreDone = o.Done == o.Of
	return o
}

// tips: what each project page shows, for people who are new.
var tips = map[string]string{
	"Overview": "Your app at a glance: visitors, sessions and page views, where visitors come from, and their devices and countries. These numbers update about once an hour.",
	"Issues":   "Every problem your visitors hit, most important first. Each one has a plain explanation and a fix message for your AI builder. \"Came back\" means a fix didn't hold; \"Fixed\" means Fix Assistant confirmed it stopped.",
	"Issue":    "What broke, who is affected and why, then the fix message to paste into your AI builder. After publishing the fix, click \"I applied the fix\": Fix Assistant watches the next version and tells you if the error comes back.",
	"Pages":    "Each page of your app: how many people view it, how many leave from it (high numbers on a sign-up page are worth a look), how fast it loads, and its open problems.",
	"Funnels":  "The steps you want visitors to complete, such as home, sign-up, then purchase. You see how many make it through each step and where errors get in the way.",
	"Insights": "Up to three ideas each week to get more sign-ups, each with a prompt for your AI builder. After your next release, Fix Assistant checks whether it helped.",
	"Alerts":   "Every email Fix Assistant sent you: urgent alerts for important pages (at most 3 a day), daily summaries and weekly growth ideas.",
	"Setup":    "Everything that connects your app: the snippet for your website, server errors, your code on GitHub, and AI agents that fix issues for you.",
}

func tipFor(title string) string { return tips[title] }

// withOnboarding adds the checklist to a project page's data.
func (a *App) withOnboarding(p *page) {
	m, ok := p.Data.(map[string]any)
	if !ok {
		return
	}
	proj, ok := m["Project"].(db.GetOrgProjectRow)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if o := a.onboarding(ctx, proj); o != nil {
		m["Onboarding"] = o
	}
}

func (a *App) dismissOnboarding(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok {
		return
	}
	if err := db.New(a.Pool).DismissOnboarding(r.Context(), p.ID); err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, backTo(r, "/projects/"+strconv.FormatInt(p.ID, 10)+"/overview"), http.StatusSeeOther)
}

func (a *App) setTips(w http.ResponseWriter, r *http.Request) {
	s := sessionFrom(r)
	if err := db.New(a.Pool).SetTipsHidden(r.Context(), db.SetTipsHiddenParams{ID: s.UserID, Hidden: r.PostFormValue("show") != "1"}); err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, backTo(r, "/projects"), http.StatusSeeOther)
}

// backTo is the page the form was on (same site only), else fallback.
func backTo(r *http.Request, fallback string) string {
	ref, err := url.Parse(r.Referer())
	if err != nil || ref.Host != r.Host || !strings.HasPrefix(ref.Path, "/") || strings.HasPrefix(ref.Path, "//") {
		return fallback
	}
	if ref.RawQuery != "" {
		return ref.Path + "?" + ref.RawQuery
	}
	return ref.Path
}

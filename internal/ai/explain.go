package ai

import (
	"fmt"
	"strings"
	"time"

	"github.com/yourname/fixassist/internal/store/db"
)

// Explain returns the suggestion for an issue: a catalog entry when the
// error is well known, otherwise a diagnose-and-fix prompt that hands the
// code-level work to the founder's agent with all the evidence we have.
func Explain(c *Context) Suggestion {
	var s Suggestion
	matched := false
	for _, e := range catalog {
		if e.match(c) {
			s = e.build(c)
			s.Origin, s.CatalogKey = "catalog", e.key
			matched = true
			break
		}
	}
	if !matched {
		if c.srv() != nil {
			s = genericServer(c)
		} else {
			s = generic(c)
		}
	}
	s.WhoIsAffected = who(c)
	s.FixPrompt += stackLocations(c.srv())
	s.FixPrompt += whereToLook(c.CodeRefs)
	if c.Attempt > 1 {
		s.FixPrompt = fmt.Sprintf("A previous fix for this problem was applied, but the error is still happening (attempt %d). Before changing anything, look again at the cause; the first fix may have targeted the wrong place.\n\n", c.Attempt) + s.FixPrompt
		s.FixPrompt += previousAttempts(c.Previous)
		s.Confidence *= 0.8
	}
	return s
}

// previousAttempts lists what was already tried, so it isn't repeated.
func previousAttempts(list []PrevAttempt) string {
	var b strings.Builder
	for _, a := range list {
		if a.Summary == "" && len(a.Files) == 0 {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("\n\nAlready tried, and the error still happened afterwards (don't repeat these; find what they missed):")
		}
		fmt.Fprintf(&b, "\n- Attempt %d", a.N)
		if a.Summary != "" {
			fmt.Fprintf(&b, ": %s", truncate(strings.Join(strings.Fields(a.Summary), " "), 400))
		}
		if len(a.Files) > 0 {
			fmt.Fprintf(&b, " (changed %s)", strings.Join(a.Files, ", "))
		}
	}
	return b.String()
}

func plural(n int64, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// who is "who is affected", from data only.
func who(c *Context) string {
	var b strings.Builder
	people := plural(c.Users, "person", "people")
	if c.Users == 0 {
		people = "Visitors"
	}
	fmt.Fprintf(&b, "%s hit this %s on %s", people, plural(c.Events, "time", "times"), c.page())
	if c.Events24h > 0 && c.Events24h < c.Events {
		fmt.Fprintf(&b, " (%s in the last 24 hours)", plural(c.Events24h, "time", "times"))
	}
	b.WriteString(".")
	if !c.FirstSeen.IsZero() {
		fmt.Fprintf(&b, " It first happened on %s", c.FirstSeen.UTC().Format("2 Jan 2006"))
		if c.FirstRelease != "" {
			fmt.Fprintf(&b, " (app version %s)", shortRelease(c.FirstRelease))
		}
		b.WriteString(".")
	}
	if c.Regressions > 0 {
		b.WriteString(" It came back after being marked fixed.")
	}
	if c.KeyPage {
		b.WriteString(" This page matters for sign-ups or sales.")
	}
	return b.String()
}

// shortRelease is a release as people know it: without our "server:" marker,
// and long build hashes cut.
func shortRelease(r string) string {
	r = strings.TrimPrefix(r, "server:")
	if len(r) > 20 {
		return r[:20]
	}
	return r
}

// generic builds the prompt for errors outside the catalog.
func generic(c *Context) Suggestion {
	s := Suggestion{Origin: "agent", Severity: c.severity("medium"), Confidence: 0.4, Title: c.Title}
	var ev strings.Builder
	switch {
	case c.net() != nil:
		n := c.net()
		if n.Status == 0 {
			s.WhatBroke = fmt.Sprintf("On %s, a request to %s%s fails without any response.", c.page(), n.Host, n.Path)
		} else {
			s.WhatBroke = fmt.Sprintf("On %s, a request to %s%s fails with HTTP %d.", c.page(), n.Host, n.Path, n.Status)
		}
		for i, x := range c.Net {
			fmt.Fprintf(&ev, "Example %d: %s %s%s → HTTP %d", i+1, x.Method, x.Host, x.Path, x.Status)
			if x.Code != "" {
				fmt.Fprintf(&ev, ", error code %s", x.Code)
			}
			if x.Message != "" {
				fmt.Fprintf(&ev, ", message \"%s\"", x.Message)
			} else if x.Error != "" {
				fmt.Fprintf(&ev, ", browser error \"%s\"", x.Error)
			} else if x.Body != "" {
				fmt.Fprintf(&ev, ", response \"%s\"", truncate(x.Body, 300))
			}
			if x.Details != "" {
				fmt.Fprintf(&ev, ", details \"%s\"", x.Details)
			}
			if x.Hint != "" {
				fmt.Fprintf(&ev, ", hint \"%s\"", x.Hint)
			}
			ev.WriteString("\n")
		}
		s.HowToCheck = fmt.Sprintf("Repeat the action on %s with the browser's network tab open. The request to %s should succeed.", c.page(), c.net().Path)
	case c.js() != nil:
		j := c.js()
		s.WhatBroke = fmt.Sprintf("On %s, the page hits the error \"%s: %s\".", c.page(), j.Type, j.Message)
		for i, x := range c.JS {
			fmt.Fprintf(&ev, "Example %d: %s: %s", i+1, x.Type, x.Message)
			if x.Function != "" {
				fmt.Fprintf(&ev, " (near code named `%s`)", x.Function)
			}
			ev.WriteString("\n")
		}
		s.HowToCheck = fmt.Sprintf("Open %s and repeat what people were doing. The error should not appear in the browser console.", c.page())
	default:
		s.WhatBroke = fmt.Sprintf("On %s, the app reported: \"%s\".", c.page(), c.Message)
		fmt.Fprintf(&ev, "Message: %s\n", c.Message)
		s.HowToCheck = fmt.Sprintf("Open %s and repeat what people were doing. The message should not be reported again.", c.page())
	}
	if s.Title == "" {
		s.Title = s.WhatBroke
	}
	backend := ""
	if c.Backend == "supabase" || c.Backend == "firebase" {
		backend = fmt.Sprintf(" The app uses %s.", capFirst(c.Backend))
	}
	s.FixPrompt = fmt.Sprintf("Users of my app hit this error on %s (%s).%s\n\n%s\nFind the cause in the code for %s. First explain in one sentence what is wrong, then fix only that, with the smallest change that works. Don't rewrite components or change unrelated code.",
		c.page(), plural(c.Events, "time", "times"), backend, ev.String(), c.page())
	return s
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ForAgent wraps the base prompt in the founder's agent style (FDR: one base
// prompt, a small adapter per agent). Lovable, Bolt, Replit and v0 get
// product-level framing; Cursor gets codebase-search framing.
func ForAgent(agent string, s Suggestion) string {
	var b strings.Builder
	switch agent {
	case "cursor":
		b.WriteString("Fix this production error in this codebase.\n\n")
		b.WriteString(s.FixPrompt)
		b.WriteString("\n\nSearch the codebase for the table, column, property or URL named above to find the exact place. Make one focused change and show me the diff.")
	case "other", "":
		b.WriteString(s.FixPrompt)
	default: // lovable, bolt, replit, v0
		b.WriteString("Please fix this problem in my app.\n\n")
		b.WriteString(s.FixPrompt)
		b.WriteString("\n\nOnly make this one change.")
	}
	b.WriteString("\n\nTo check it worked: ")
	b.WriteString(s.HowToCheck)
	b.WriteString("\n\nWhen you're done, tell me in one sentence what was wrong and what you changed.")
	return b.String()
}

// Now is replaceable in tests.
var Now = time.Now

// FromRow turns a stored suggestion back into a Suggestion (for rendering).
func FromRow(r db.FixSuggestion) Suggestion {
	c, _ := r.Confidence.Float64Value()
	return Suggestion{
		Origin: r.Origin, CatalogKey: r.CatalogKey.String, Severity: r.Severity, Title: r.Title,
		WhatBroke: r.WhatBroke, WhoIsAffected: r.WhoIsAffected, Why: r.Why, FixPrompt: r.FixPrompt,
		HowToCheck: r.HowToCheck, Confidence: c.Float64,
	}
}

// PromptForRow renders a stored suggestion for an agent (alerts.PromptFor).
func PromptForRow(agent string, r db.FixSuggestion) string { return ForAgent(agent, FromRow(r)) }

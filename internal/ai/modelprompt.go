package ai

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ModelPromptVersion is stored with suggestions our model wrote, so a change
// to the system prompt or the evidence format can be compared in evals and
// on the admin page.
const ModelPromptVersion = "model-v1"

// ModelSystem is the system prompt. Fine-tuning data is exported with the
// same text, so the model sees at serving time exactly what it trained on.
const ModelSystem = `You explain errors in web apps to their founders, who are usually not programmers and build with AI coding agents (Lovable, Bolt, Replit, v0, Cursor).
You get the evidence we captured for one error. Reply with JSON only:
- severity: "low", "medium" or "high" (high: sign-up, login, payment or data loss; low: cosmetic)
- title: a short plain-English name for the problem
- what_broke: one or two sentences a non-programmer understands
- why: the most likely cause, from the evidence; "" if the evidence doesn't show it
- fix_prompt: instructions for the founder's AI coding agent. Quote the exact error and where it happens, name the likely cause, ask for exactly one focused change, and tell it to check the cause in the code first if unsure. Never invent file names, tables or columns that aren't in the evidence.
- how_to_check: how the founder confirms the fix worked, in one or two plain sentences`

// SuggestionSchema constrains the model's reply (Ollama "format").
var SuggestionSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "severity": {"type": "string", "enum": ["low", "medium", "high"]},
    "title": {"type": "string"},
    "what_broke": {"type": "string"},
    "why": {"type": "string"},
    "fix_prompt": {"type": "string"},
    "how_to_check": {"type": "string"}
  },
  "required": ["severity", "title", "what_broke", "why", "fix_prompt", "how_to_check"]
}`)

// ModelOutput is the model's reply, also the assistant turn in training data.
type ModelOutput struct {
	Severity   string `json:"severity"`
	Title      string `json:"title"`
	WhatBroke  string `json:"what_broke"`
	Why        string `json:"why"`
	FixPrompt  string `json:"fix_prompt"`
	HowToCheck string `json:"how_to_check"`
}

// OutputOf is the training target for a suggestion of any origin.
func OutputOf(s Suggestion) ModelOutput {
	return ModelOutput{Severity: s.Severity, Title: s.Title, WhatBroke: s.WhatBroke, Why: s.Why, FixPrompt: s.FixPrompt, HowToCheck: s.HowToCheck}
}

// BuildModelRequest turns an issue into the model's prompt. Only evidence
// goes in (no counts, dates or project ids), so the same error in two apps
// reads the same and training examples generalise.
func BuildModelRequest(c *Context) Request {
	return Request{Tier: TierSmall, System: ModelSystem, Prompt: Evidence(c), MaxTokens: 900}
}

// Evidence is the user turn: the scrubbed facts about one issue.
func Evidence(c *Context) string {
	var b strings.Builder
	line := func(k, v string) {
		if v = strings.TrimSpace(v); v != "" {
			fmt.Fprintf(&b, "%s: %s\n", k, v)
		}
	}
	line("Kind", c.Kind)
	line("Error", c.Title)
	line("Page", c.Route)
	line("Backend", c.Backend)
	line("Coding agent", c.Agent)
	if c.Message != "" {
		line("Message", truncate(c.Message, 600))
	}
	for i, n := range c.Net {
		fmt.Fprintf(&b, "\nFailed request %d: %s %s%s -> HTTP %d\n", i+1, n.Method, n.Host, n.Path, n.Status)
		line("  backend", n.Backend)
		line("  code", n.Code)
		line("  message", n.Message)
		line("  details", n.Details)
		line("  hint", n.Hint)
		line("  browser error", n.Error)
		if n.Message == "" {
			line("  body", truncate(n.Body, 600))
		}
	}
	for i, j := range c.JS {
		fmt.Fprintf(&b, "\nPage error %d: %s: %s\n", i+1, j.Type, truncate(j.Message, 600))
		line("  function", j.Function)
		line("  property", j.Property)
		if len(j.Frames) > 0 {
			line("  stack", strings.Join(j.Frames[:min(len(j.Frames), 6)], " < "))
		}
		if j.JSONPath != "" {
			line("  bad JSON from", fmt.Sprintf("%s (HTTP %d, %s)", j.JSONPath, j.JSONStatus, j.JSONType))
		}
	}
	for i, s := range c.Server {
		fmt.Fprintf(&b, "\nServer error %d (%s)\n", i+1, s.Platform)
		line("  transaction", s.Transaction)
		for k, e := range s.Chain {
			label := "  caused by"
			if k == 0 {
				label = "  root cause"
			}
			line(label, e.Type+": "+truncate(e.Message, 500))
		}
		line("  function", s.Function)
		for _, w := range s.Where[:min(len(s.Where), 4)] {
			ref := fmt.Sprintf("%s:%d %s", w.Path, w.Line, w.Function)
			if w.Code != "" {
				ref += " | " + strings.TrimSpace(w.Code)
			}
			line("  at", ref)
		}
	}
	if len(c.CodeRefs) > 0 {
		b.WriteString("\nWhere it appears in the app's code:\n")
		for _, r := range c.CodeRefs[:min(len(c.CodeRefs), 4)] {
			fmt.Fprintf(&b, "  %s:%d %s\n", r.Path, r.Line, truncate(strings.TrimSpace(r.Snippet), 200))
		}
	}
	if len(c.Previous) > 0 {
		b.WriteString("\nFixes already tried that did NOT work:\n")
		for _, p := range c.Previous {
			fmt.Fprintf(&b, "  attempt %d: %s", p.N, truncate(strings.Join(strings.Fields(p.Summary), " "), 400))
			if len(p.Files) > 0 {
				fmt.Fprintf(&b, " (changed %s)", strings.Join(p.Files, ", "))
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// ParseModelSuggestion validates the model's JSON. Anything off is an error
// and the caller keeps the generic prompt.
func ParseModelSuggestion(text string) (Suggestion, error) {
	text = strings.TrimSpace(text)
	// Tolerate a ```json fence from models run without a format schema.
	text = strings.TrimPrefix(strings.TrimSuffix(text, "```"), "```json")
	var o ModelOutput
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &o); err != nil {
		return Suggestion{}, fmt.Errorf("model reply is not JSON: %w", err)
	}
	switch o.Severity {
	case "low", "medium", "high":
	default:
		return Suggestion{}, fmt.Errorf("model severity %q", o.Severity)
	}
	o.Title, o.WhatBroke, o.FixPrompt, o.HowToCheck = strings.TrimSpace(o.Title), strings.TrimSpace(o.WhatBroke), strings.TrimSpace(o.FixPrompt), strings.TrimSpace(o.HowToCheck)
	if o.WhatBroke == "" || o.HowToCheck == "" || len(o.FixPrompt) < 40 {
		return Suggestion{}, errors.New("model reply is missing what_broke, how_to_check or a real fix_prompt")
	}
	if len(o.FixPrompt) > 6000 || len(o.WhatBroke) > 1000 || len(o.Why) > 1500 || len(o.HowToCheck) > 1000 {
		return Suggestion{}, errors.New("model reply is too long")
	}
	return Suggestion{
		Origin: "model", Severity: o.Severity, Title: truncate(o.Title, 200), WhatBroke: o.WhatBroke,
		Why: strings.TrimSpace(o.Why), FixPrompt: o.FixPrompt, HowToCheck: o.HowToCheck, Confidence: 0.5,
	}, nil
}

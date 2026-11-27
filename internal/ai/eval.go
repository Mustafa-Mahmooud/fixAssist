package ai

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/yourname/fixassist/internal/grouping"
)

//go:embed golden/cases.json
var goldenJSON []byte

// GoldenCase is one error with what a good explanation must contain.
// The set stands in for the FDR's Phase 0 concierge cases until partners
// provide real ones.
type GoldenCase struct {
	Name      string          `json:"name"`
	Route     string          `json:"route"`
	Users     int64           `json:"users"`
	Events    int64           `json:"events"`
	Event     json.RawMessage `json:"event"`
	SpikeCase string          `json:"spike_case"` // reproducible in the spike app
	Expect    struct {
		Catalog  string   `json:"catalog"` // "" = generic agent prompt expected
		Severity string   `json:"severity"`
		Mentions []string `json:"mentions"`
		MustNot  []string `json:"must_not"`
	} `json:"expect"`
}

// GoldenCases returns the embedded golden set.
func GoldenCases() ([]GoldenCase, error) {
	var cs []GoldenCase
	err := json.Unmarshal(goldenJSON, &cs)
	return cs, err
}

// EvalResult is one case's outcome.
type EvalResult struct {
	Case       GoldenCase
	Suggestion Suggestion
	Prompt     string
	Failures   []string
}

// Pass reports whether every rubric check passed.
func (r EvalResult) Pass() bool { return len(r.Failures) == 0 }

// MaxPromptChars keeps prompts focused (FDR: one change, never a rewrite).
const MaxPromptChars = 2000

// ContextFor builds the context a golden case would have in production.
func ContextFor(gc GoldenCase, agent string) *Context {
	c := &Context{
		Kind: "js", Route: gc.Route, Agent: agent, Backend: "supabase",
		KeyPage: grouping.RouteWeight(gc.Route, nil) > 1, Users: gc.Users, Events: gc.Events,
		FirstSeen: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC), FirstRelease: "fix-0bdd6fbc7666354a", Attempt: 1,
	}
	c.AddSample(gc.Event)
	if len(c.Net) > 0 {
		c.Kind = "net"
	}
	return c
}

// Evaluate runs the rubric on one case.
func Evaluate(gc GoldenCase, agent string) EvalResult {
	return score(gc, agent, Explain(ContextFor(gc, agent)), true)
}

// ModelEvalResult is one case answered by our model, catalog bypassed.
type ModelEvalResult struct {
	EvalResult
	Err     error // the call failed or the reply wasn't valid
	Latency time.Duration
	Tokens  int
}

// EvaluateModel asks the model every golden case (the catalog is skipped,
// so known errors measure how close the model gets to the curated answer)
// and applies the same rubric, minus the catalog key.
func EvaluateModel(ctx context.Context, gc GoldenCase, agent string, m LLM) ModelEvalResult {
	c := ContextFor(gc, agent)
	res, err := m.Complete(ctx, BuildModelRequest(c))
	if err != nil {
		return ModelEvalResult{EvalResult: EvalResult{Case: gc, Failures: []string{"model call: " + err.Error()}}, Err: err}
	}
	out := ModelEvalResult{Latency: res.Latency, Tokens: res.TokensOut}
	s, err := ParseModelSuggestion(res.Text)
	if err != nil {
		out.EvalResult, out.Err = EvalResult{Case: gc, Failures: []string{"invalid reply: " + err.Error()}}, err
		return out
	}
	out.EvalResult = score(gc, agent, finish(c, s), false)
	return out
}

func score(gc GoldenCase, agent string, s Suggestion, checkCatalog bool) EvalResult {
	prompt := ForAgent(agent, s)
	r := EvalResult{Case: gc, Suggestion: s, Prompt: prompt}
	fail := func(f string, a ...any) { r.Failures = append(r.Failures, fmt.Sprintf(f, a...)) }

	if checkCatalog && s.CatalogKey != gc.Expect.Catalog {
		fail("catalog %q, want %q", s.CatalogKey, gc.Expect.Catalog)
	}
	if gc.Expect.Severity != "" && s.Severity != gc.Expect.Severity {
		fail("severity %s, want %s", s.Severity, gc.Expect.Severity)
	}
	low := strings.ToLower(prompt)
	for _, m := range gc.Expect.Mentions {
		if !strings.Contains(low, strings.ToLower(m)) {
			fail("prompt doesn't mention %q", m)
		}
	}
	for _, m := range gc.Expect.MustNot {
		if strings.Contains(low, strings.ToLower(m)) {
			fail("prompt contains forbidden %q", m)
		}
	}
	if s.WhatBroke == "" || s.WhoIsAffected == "" || s.HowToCheck == "" {
		fail("missing what broke / who is affected / how to check")
	}
	if !strings.Contains(prompt, "To check it worked:") {
		fail("prompt doesn't end with how to check")
	}
	if len(prompt) > MaxPromptChars {
		fail("prompt too long (%d chars)", len(prompt))
	}
	if strings.Contains(low, "rewrite the") && !strings.Contains(low, "don't rewrite") {
		fail("prompt asks for a rewrite")
	}
	return r
}

// RunEval evaluates the whole golden set for one agent style.
func RunEval(agent string) ([]EvalResult, error) {
	cs, err := GoldenCases()
	if err != nil {
		return nil, err
	}
	out := make([]EvalResult, 0, len(cs))
	for _, gc := range cs {
		out = append(out, Evaluate(gc, agent))
	}
	return out, nil
}

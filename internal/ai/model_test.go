package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const goodReply = `{"severity":"high","title":"Checkout crashes on the flux setting","what_broke":"Checkout stops with an error.","why":"The flux value is missing.","fix_prompt":"On /checkout the page throws FluxError: flux capacitor overheated in applyFlux. Find where applyFlux reads the flux value and handle the missing case. Change only that.","how_to_check":"Open /checkout and place a test order."}`

type fakeLLM struct {
	text  string
	err   error
	calls int
	req   Request
}

func (f *fakeLLM) Complete(_ context.Context, req Request) (Response, error) {
	f.calls++
	f.req = req
	return Response{Text: f.text, Model: "fake:1", TokensIn: 10, TokensOut: 20, Latency: time.Second}, f.err
}

func unknownJS() *Context {
	return &Context{Kind: "js", Title: "FluxError: flux capacitor overheated", Route: "/checkout", Agent: "lovable", Attempt: 1,
		JS: []JSSample{{Type: "FluxError", Message: "flux capacitor overheated", Function: "applyFlux"}}}
}

func TestParseModelSuggestion(t *testing.T) {
	s, err := ParseModelSuggestion("```json\n" + goodReply + "\n```")
	if err != nil {
		t.Fatal(err)
	}
	if s.Origin != "model" || s.Severity != "high" || !strings.Contains(s.FixPrompt, "applyFlux") {
		t.Fatalf("got %+v", s)
	}
	bad := map[string]string{
		"not json":     "the cause is flux",
		"bad severity": strings.Replace(goodReply, `"high"`, `"critical"`, 1),
		"short prompt": `{"severity":"low","title":"x","what_broke":"x","why":"","fix_prompt":"fix it","how_to_check":"x"}`,
		"missing":      `{"severity":"low","title":"x"}`,
	}
	for name, text := range bad {
		if _, err := ParseModelSuggestion(text); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestExplainWithModel(t *testing.T) {
	c := unknownJS()
	if Explain(c).Origin != "agent" {
		t.Fatal("fixture should be outside the catalog")
	}
	m := &fakeLLM{text: goodReply}
	s, mr, err := ExplainWithModel(context.Background(), c, m)
	if err != nil || mr == nil || s.Origin != "model" || mr.Model != "fake:1" {
		t.Fatalf("s=%+v mr=%+v err=%v", s, mr, err)
	}
	if s.WhoIsAffected == "" {
		t.Error("finish() not applied to model output")
	}
	if !strings.Contains(m.req.Prompt, "applyFlux") || m.req.System != ModelSystem {
		t.Errorf("prompt is missing the evidence: %q", m.req.Prompt)
	}

	// Model down or answering nonsense: the generic prompt, not an error to the caller's flow.
	for _, f := range []*fakeLLM{{err: errors.New("down")}, {text: "nope"}} {
		s, mr, err := ExplainWithModel(context.Background(), c, f)
		if err == nil || mr != nil || s.Origin != "agent" || s.FixPrompt == "" {
			t.Errorf("fallback: s.Origin=%s mr=%v err=%v", s.Origin, mr, err)
		}
	}

	// No model: unchanged.
	if s, _, _ := ExplainWithModel(context.Background(), c, nil); s.Origin != "agent" {
		t.Error("nil model changed the result")
	}
}

func TestExplainWithModelSkipsCatalog(t *testing.T) {
	// A catalog hit never reaches the model.
	c := &Context{Kind: "js", Title: "TypeError", Attempt: 1, JS: []JSSample{{Type: "TypeError", Message: "Cannot read properties of null (reading 'name')", Property: "name"}}}
	if Explain(c).Origin != "catalog" {
		t.Skip("fixture no longer a catalog hit")
	}
	m := &fakeLLM{text: goodReply}
	if s, _, _ := ExplainWithModel(context.Background(), c, m); s.Origin != "catalog" || m.calls != 0 {
		t.Fatalf("origin=%s calls=%d", s.Origin, m.calls)
	}
}

func TestOllamaClient(t *testing.T) {
	var got ollamaReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "qwen3.5:4b", "message": map[string]string{"role": "assistant", "content": goodReply}, "prompt_eval_count": 300, "eval_count": 120})
	}))
	defer srv.Close()

	o := NewOllama(srv.URL+"/", "qwen3.5:4b", "tok", 5*time.Second)
	res, err := o.Complete(context.Background(), BuildModelRequest(unknownJS()))
	if err != nil {
		t.Fatal(err)
	}
	if res.TokensIn != 300 || res.TokensOut != 120 || res.Model != "qwen3.5:4b" {
		t.Errorf("res %+v", res)
	}
	if got.Stream || got.Think || len(got.Format) == 0 || len(got.Messages) != 2 {
		t.Errorf("request %+v", got)
	}

	o.Token = "wrong"
	if _, err := o.Complete(context.Background(), BuildModelRequest(unknownJS())); err == nil {
		t.Error("want error on 401")
	}
}

// TestOllamaLive runs against a real model: FIX_MODEL_URL=http://localhost:11434
// FIX_MODEL_NAME=qwen3.5:4b go test ./internal/ai -run Live -v
func TestOllamaLive(t *testing.T) {
	url := os.Getenv("FIX_MODEL_URL")
	if url == "" {
		t.Skip("FIX_MODEL_URL not set")
	}
	name := os.Getenv("FIX_MODEL_NAME")
	if name == "" {
		name = "qwen3.5:4b"
	}
	o := NewOllama(url, name, os.Getenv("FIX_MODEL_TOKEN"), 5*time.Minute)
	s, mr, err := ExplainWithModel(context.Background(), unknownJS(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s in %s, %d+%d tokens\nseverity: %s\ntitle: %s\nwhat broke: %s\nwhy: %s\nprompt: %s\ncheck: %s",
		mr.Model, mr.Latency, mr.TokensIn, mr.TokensOut, s.Severity, s.Title, s.WhatBroke, s.Why, s.FixPrompt, s.HowToCheck)
}

// TestGoldenModelLive is `app eval --model-url` without the binary:
// FIX_MODEL_URL=http://localhost:11434 go test ./internal/ai -run GoldenModelLive -v -timeout 60m
func TestGoldenModelLive(t *testing.T) {
	url := os.Getenv("FIX_MODEL_URL")
	if url == "" {
		t.Skip("FIX_MODEL_URL not set")
	}
	name := os.Getenv("FIX_MODEL_NAME")
	if name == "" {
		name = "qwen3.5:4b"
	}
	cases, err := GoldenCases()
	if err != nil {
		t.Fatal(err)
	}
	m := NewOllama(url, name, os.Getenv("FIX_MODEL_TOKEN"), 5*time.Minute)
	passed, valid := 0, 0
	for _, gc := range cases {
		r := EvaluateModel(context.Background(), gc, "lovable", m)
		if r.Err == nil {
			valid++
		}
		if r.Pass() {
			passed++
		}
		t.Logf("%-4v %-32s catalog=%-24q sev=%-6s %3.0fs %s", r.Pass(), gc.Name, gc.Expect.Catalog, r.Suggestion.Severity, r.Latency.Seconds(), strings.Join(r.Failures, "; "))
	}
	t.Logf("%s: %d/%d pass, %d/%d valid", name, passed, len(cases), valid, len(cases))
}

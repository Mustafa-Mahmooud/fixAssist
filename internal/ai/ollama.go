package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Ollama is an LLM served by Ollama's native chat API (our own model host,
// or `ollama serve` on a laptop). The host is expected to sit behind a
// reverse proxy that checks Token; Ollama itself has no auth.
type Ollama struct {
	URL     string // e.g. https://model.example.com or http://localhost:11434
	Model   string // e.g. qwen3.5:4b, later errfix-qwen:v1
	Token   string // bearer token for the proxy; empty for localhost
	Timeout time.Duration
	// Format constrains the reply (a JSON schema); nil means free text.
	Format json.RawMessage
	HTTP   *http.Client

	sem chan struct{}
}

// NewOllama returns a client that runs one request at a time: a CPU model
// host only gets slower with parallel requests.
func NewOllama(url, model, token string, timeout time.Duration) *Ollama {
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	return &Ollama{
		URL: strings.TrimRight(url, "/"), Model: model, Token: token, Timeout: timeout,
		Format: SuggestionSchema, HTTP: &http.Client{}, sem: make(chan struct{}, 1),
	}
}

type ollamaMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaReq struct {
	Model    string          `json:"model"`
	Messages []ollamaMsg     `json:"messages"`
	Stream   bool            `json:"stream"`
	Think    bool            `json:"think"`
	Format   json.RawMessage `json:"format,omitempty"`
	Options  map[string]any  `json:"options,omitempty"`
}

type ollamaResp struct {
	Model           string    `json:"model"`
	Message         ollamaMsg `json:"message"`
	PromptEvalCount int       `json:"prompt_eval_count"`
	EvalCount       int       `json:"eval_count"`
	Error           string    `json:"error"`
}

// Complete implements LLM.
func (o *Ollama) Complete(ctx context.Context, req Request) (Response, error) {
	if o.sem != nil {
		select {
		case o.sem <- struct{}{}:
			defer func() { <-o.sem }()
		case <-ctx.Done():
			return Response{}, ctx.Err()
		}
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	opts := map[string]any{"temperature": 0.2}
	if req.MaxTokens > 0 {
		opts["num_predict"] = req.MaxTokens
	}
	body, err := json.Marshal(ollamaReq{
		Model: o.Model, Stream: false, Think: false, Format: o.Format, Options: opts,
		Messages: []ollamaMsg{{Role: "system", Content: req.System}, {Role: "user", Content: req.Prompt}},
	})
	if err != nil {
		return Response{}, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, o.URL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	if o.Token != "" {
		hr.Header.Set("Authorization", "Bearer "+o.Token)
	}
	client := o.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	start := time.Now()
	res, err := client.Do(hr)
	if err != nil {
		return Response{}, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return Response{}, err
	}
	var out ollamaResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return Response{}, fmt.Errorf("ollama: HTTP %d: %s", res.StatusCode, truncate(string(raw), 200))
	}
	if res.StatusCode != http.StatusOK || out.Error != "" {
		return Response{}, fmt.Errorf("ollama: HTTP %d: %s", res.StatusCode, out.Error)
	}
	if strings.TrimSpace(out.Message.Content) == "" {
		return Response{}, errors.New("ollama: empty reply")
	}
	model := out.Model
	if model == "" {
		model = o.Model
	}
	return Response{
		Text: out.Message.Content, Model: model, TokensIn: out.PromptEvalCount, TokensOut: out.EvalCount,
		Latency: time.Since(start),
	}, nil
}

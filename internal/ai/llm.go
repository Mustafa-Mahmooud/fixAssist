package ai

import (
	"context"
	"time"
)

// The FDR's model interfaces. Phase 4 runs in agent mode and doesn't call a
// model; an adapter per vendor implements these when our own model is
// switched on (model names and prices from config, not code).

// Tier picks the small or large model.
type Tier int

const (
	TierSmall Tier = iota
	TierLarge
)

// Request is one completion call.
type Request struct {
	Tier      Tier
	System    string
	Prompt    string
	MaxTokens int
}

// Response carries the text plus what to log in fix_suggestions.
type Response struct {
	Text      string
	Model     string
	TokensIn  int
	TokensOut int
	CostUSD   float64
	Latency   time.Duration
}

// LLM completes prompts.
type LLM interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// Embedder turns signatures into vectors for the Phase 7 semantic cache.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

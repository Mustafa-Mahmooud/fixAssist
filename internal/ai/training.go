package ai

import (
	"encoding/json"
	"regexp"

	"github.com/yourname/fixassist/internal/ingest"
)

// Training rows are scrubbed again, on top of ingest's scrubbing: they are
// kept longer than events and leave the database for fine-tuning.
var (
	reTrainIPv4 = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	reTrainIPv6 = regexp.MustCompile(`\b(?:[0-9a-fA-F]{1,4}:){4,7}[0-9a-fA-F]{1,4}\b`)
	// Well-known key formats: Stripe, OpenAI/Anthropic, AWS, GitHub, Google, Slack.
	reTrainKey = regexp.MustCompile(`\b(?:sk|pk|rk)_(?:live|test)_[A-Za-z0-9]{8,}|\bsk-[A-Za-z0-9_-]{16,}|\bAKIA[0-9A-Z]{16}\b|\bgh[pousr]_[A-Za-z0-9]{20,}|\bAIza[0-9A-Za-z_-]{30,}|\bxox[abpr]-[A-Za-z0-9-]{10,}`)
	// Long opaque strings (hex or base64-ish) that are probably secrets or ids.
	reTrainOpaque = regexp.MustCompile(`\b[A-Za-z0-9+/_-]{40,}={0,2}`)
)

// TrainingScrub removes personal data and secrets from training text.
func TrainingScrub(s string) string {
	s = ingest.ScrubString(s)
	s = reTrainKey.ReplaceAllString(s, "[key]")
	s = reTrainIPv4.ReplaceAllString(s, "[ip]")
	s = reTrainIPv6.ReplaceAllString(s, "[ip]")
	return reTrainOpaque.ReplaceAllString(s, "[redacted]")
}

// TrainingInput is the user turn for fine-tuning: the evidence, scrubbed.
func TrainingInput(c *Context) string { return TrainingScrub(Evidence(c)) }

// TrainingOutput is the assistant turn: the explanation as the model would
// write it, scrubbed.
func TrainingOutput(s Suggestion) []byte {
	o := OutputOf(s)
	o.Title, o.WhatBroke, o.Why = TrainingScrub(o.Title), TrainingScrub(o.WhatBroke), TrainingScrub(o.Why)
	o.FixPrompt, o.HowToCheck = TrainingScrub(o.FixPrompt), TrainingScrub(o.HowToCheck)
	b, _ := json.Marshal(o)
	return b
}

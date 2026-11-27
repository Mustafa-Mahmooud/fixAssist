package ai

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"io"

	"github.com/yourname/fixassist/internal/store/db"
)

// Fine-tuning exports, in the chat format Unsloth/TRL read:
//
//	sft: {"messages":[system, user, assistant]}
//	dpo: {"prompt":[system, user], "chosen":[assistant], "rejected":[assistant]}

// ChatMsg is one chat turn.
type ChatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// SFTExample is one supervised example.
type SFTExample struct {
	Messages []ChatMsg `json:"messages"`
	Meta     ExMeta    `json:"meta"` // ignored by trainers; for filtering and audits
}

// DPOExample is one preference pair.
type DPOExample struct {
	Prompt   []ChatMsg `json:"prompt"`
	Chosen   []ChatMsg `json:"chosen"`
	Rejected []ChatMsg `json:"rejected"`
	Meta     ExMeta    `json:"meta"`
}

// ExMeta says where an example came from.
type ExMeta struct {
	Source    string `json:"source"` // db, golden, catalog_tests
	Origin    string `json:"origin"`
	Key       string `json:"key,omitempty"`
	Signature string `json:"signature,omitempty"`
	Outcome   string `json:"outcome,omitempty"`
}

func prompt(input string) []ChatMsg {
	return []ChatMsg{{Role: "system", Content: ModelSystem}, {Role: "user", Content: input}}
}

// SFTFromContext is a bootstrap example: the catalog's answer for a
// context, before any real outcome exists. ok is false outside the catalog.
func SFTFromContext(c *Context, source string) (SFTExample, bool) {
	s := explainCore(c)
	if s.Origin != "catalog" {
		return SFTExample{}, false
	}
	return SFTExample{
		Messages: append(prompt(TrainingInput(c)), ChatMsg{Role: "assistant", Content: string(TrainingOutput(s))}),
		Meta:     ExMeta{Source: source, Origin: "catalog", Key: s.CatalogKey, Signature: c.Signature},
	}, true
}

// goodForSFT: a fix that worked, or a catalog answer nobody reported
// failing (the catalog is curated). Generic prompts are never targets: the
// model should learn to be specific.
func goodForSFT(r db.TrainingExample) bool {
	if r.Origin == "agent" {
		return false
	}
	switch r.Outcome.String {
	case "worked":
		return true
	case "":
		return r.Origin == "catalog" || r.Origin == "cache"
	}
	return false
}

// SFTFromRows picks the supervised examples from training rows.
func SFTFromRows(rows []db.TrainingExample) []SFTExample {
	var out []SFTExample
	seen := map[string]bool{}
	for _, r := range rows {
		if !goodForSFT(r) {
			continue
		}
		k := r.Input + "\x00" + string(r.Output)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, SFTExample{
			Messages: append(prompt(r.Input), ChatMsg{Role: "assistant", Content: string(r.Output)}),
			Meta:     ExMeta{Source: "db", Origin: r.Origin, Key: r.CatalogKey.String, Signature: r.Signature, Outcome: r.Outcome.String},
		})
	}
	return out
}

// DPOFromRows pairs, per signature, an answer that failed with a later one
// that worked. The prompt is the failed one's evidence: the question both
// answers were (in effect) given.
func DPOFromRows(rows []db.TrainingExample) []DPOExample {
	bySig := map[string][]db.TrainingExample{}
	var order []string
	for _, r := range rows {
		if _, ok := bySig[r.Signature]; !ok {
			order = append(order, r.Signature)
		}
		bySig[r.Signature] = append(bySig[r.Signature], r)
	}
	var out []DPOExample
	for _, sig := range order {
		list := bySig[sig] // ordered by created_at
		for i, bad := range list {
			if bad.Outcome.String != "failed" {
				continue
			}
			for _, good := range list[i+1:] {
				if good.Outcome.String != "worked" || good.Origin == "agent" {
					continue
				}
				out = append(out, DPOExample{
					Prompt:   prompt(bad.Input),
					Chosen:   []ChatMsg{{Role: "assistant", Content: string(good.Output)}},
					Rejected: []ChatMsg{{Role: "assistant", Content: string(bad.Output)}},
					Meta:     ExMeta{Source: "db", Origin: good.Origin, Signature: sig},
				})
				break
			}
		}
	}
	return out
}

// Holdout puts an example in the eval split by its signature, so one error
// pattern is never in both splits. frac is 0..1.
func Holdout(signature, fallback string, frac float64) bool {
	if frac <= 0 {
		return false
	}
	k := signature
	if k == "" {
		k = fallback
	}
	h := sha256.Sum256([]byte(k))
	return float64(binary.BigEndian.Uint32(h[:4]))/float64(^uint32(0)) < frac
}

// WriteJSONL writes one JSON value per line.
func WriteJSONL[T any](w io.Writer, items []T) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, it := range items {
		if err := enc.Encode(it); err != nil {
			return err
		}
	}
	return nil
}

package ai

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
)

// Knowledge reuse (FDR Phase 7, agent mode): the same error pattern across
// apps shares one track record. A pattern whose fix worked often is reused
// as "proven"; one that keeps failing is quarantined.
const (
	// ProvenMinWorked and ProvenMinConfidence mark a pattern's fix as proven.
	ProvenMinWorked     = 2
	ProvenMinConfidence = 0.6
	// SimilarMinScore is the pg_trgm similarity for "similar errors in other apps".
	SimilarMinScore = 0.5
)

// projectHash counts distinct apps per pattern without storing project ids.
func projectHash(projectID int64) string {
	sum := sha256.Sum256([]byte("kb-sighting|" + strconv.FormatInt(projectID, 10)))
	return hex.EncodeToString(sum[:12])
}

// QuarantineFallback replaces a quarantined catalog fix with a
// diagnose-and-fix prompt that says the standard fix didn't work elsewhere.
func QuarantineFallback(c *Context) Suggestion {
	s := generic(c)
	s.WhoIsAffected = who(c)
	s.Why = "The usual fix for this error didn't work for most apps that tried it, so the cause is probably specific to your code."
	s.FixPrompt = "The standard fix for this error didn't work for most apps that tried it, so look for the cause in this app's own code before changing anything.\n\n" + s.FixPrompt
	s.Confidence = 0.3
	return s
}

// CodeRef points at the code a prompt is about (GitHub App, Phase 7).
type CodeRef struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Snippet string `json:"snippet"`
	Term    string `json:"term"`
}

func codeRefsJSON(refs []CodeRef) []byte {
	if len(refs) == 0 {
		return nil
	}
	b, _ := json.Marshal(refs)
	return b
}

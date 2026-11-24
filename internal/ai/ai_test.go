package ai

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yourname/fixassist/internal/ingest"
)

// The golden-set rubric must pass for every agent style.
func TestGoldenSet(t *testing.T) {
	for _, agent := range []string{"lovable", "bolt", "cursor", "replit", "v0", "other"} {
		results, err := RunEval(agent)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) < 20 {
			t.Fatalf("golden set has %d cases", len(results))
		}
		for _, r := range results {
			if !r.Pass() {
				t.Errorf("[%s] %s: %s\n--- prompt ---\n%s", agent, r.Case.Name, strings.Join(r.Failures, "; "), r.Prompt)
			}
		}
	}
}

// Real captured envelopes (Phase 1/2 golden files) through Prepare, exactly as
// stored, must get the same explanations as the hand-written cases.
func TestCapturedSpikeEnvelopes(t *testing.T) {
	files, _ := filepath.Glob("../ingest/testdata/envelopes/*.envelope")
	if len(files) == 0 {
		t.Skip("no captured envelopes")
	}
	want := map[string]string{
		"js_typeerror": "js.null_property", "js_custom": "", "js_rejection": "",
		"net_22001": "pg.value_too_long", "net_42501": "pg.rls_denied", "net_auth": "auth.invalid_credentials",
		"net_404": "pg.missing_table", "net_dead": "net.local_address",
	}
	seen := map[string]bool{}
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		env, err := ingest.ParseEnvelope(raw)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range env.Items {
			if it.Header.Type != "event" {
				continue
			}
			row, err := ingest.Prepare([]byte("k"), it.Payload, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			ev, _ := ingest.ParseEvent(row.Payload)
			kase := ev.Tags["spike_case"]
			c := &Context{Kind: row.Kind, Route: row.Route, Agent: "lovable", Backend: "supabase", Users: 1, Events: 1, Attempt: 1}
			c.AddSample(row.Payload)
			s := Explain(c)
			if s.CatalogKey != want[kase] {
				t.Errorf("%s: catalog %q, want %q", kase, s.CatalogKey, want[kase])
			}
			seen[kase] = true
		}
	}
	if len(seen) != len(want) {
		t.Errorf("cases seen: %v", seen)
	}
}

func TestSecondAttemptSaysFirstFixFailed(t *testing.T) {
	cs, _ := GoldenCases()
	c := ContextFor(cs[3], "lovable")
	c.Attempt = 2
	s := Explain(c)
	if !strings.HasPrefix(s.FixPrompt, "A previous fix for this problem was applied") || s.Confidence >= 0.85 {
		t.Errorf("attempt 2: %q (confidence %v)", s.FixPrompt[:60], s.Confidence)
	}
}

func TestWhoIsAffected(t *testing.T) {
	c := &Context{Route: "/checkout", Users: 3, Events: 7, Events24h: 2, KeyPage: true, Regressions: 1,
		FirstSeen: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC), FirstRelease: "fix-0bdd6fbc7666354a"}
	got := who(c)
	for _, want := range []string{"3 people hit this 7 times on the /checkout page", "2 times in the last 24 hours", "20 Nov 2026", "fix-0bdd6fbc", "came back", "sign-ups or sales"} {
		if !strings.Contains(got, want) {
			t.Errorf("who() = %q, missing %q", got, want)
		}
	}
}

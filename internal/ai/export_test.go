package ai

import (
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yourname/fixassist/internal/store/db"
)

func txt(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func row(sig, origin, outcome, out string) db.TrainingExample {
	return db.TrainingExample{Signature: sig, Origin: origin, Outcome: txt(outcome), Input: "evidence " + sig, Output: []byte(out)}
}

func TestSFTFromRows(t *testing.T) {
	rows := []db.TrainingExample{
		row("a", "model", "worked", `{"x":1}`),
		row("a", "model", "worked", `{"x":1}`), // duplicate
		row("b", "model", "failed", `{"x":2}`),
		row("c", "agent", "worked", `{"x":3}`), // generic prompt: never a target
		row("d", "catalog", "", `{"x":4}`),     // curated, no outcome yet
		row("e", "model", "", `{"x":5}`),       // unproven model answer
	}
	got := SFTFromRows(rows)
	if len(got) != 2 || got[0].Meta.Signature != "a" || got[1].Meta.Signature != "d" {
		t.Fatalf("got %+v", got)
	}
	if m := got[0].Messages; len(m) != 3 || m[0].Content != ModelSystem || m[2].Role != "assistant" {
		t.Errorf("messages %+v", m)
	}
}

func TestDPOFromRows(t *testing.T) {
	rows := []db.TrainingExample{
		row("a", "model", "failed", `{"bad":1}`),
		row("a", "agent", "worked", `{"generic":1}`), // not a chosen answer
		row("a", "model", "worked", `{"good":1}`),
		row("b", "model", "worked", `{"only":1}`),
	}
	got := DPOFromRows(rows)
	if len(got) != 1 || got[0].Chosen[0].Content != `{"good":1}` || got[0].Rejected[0].Content != `{"bad":1}` {
		t.Fatalf("got %+v", got)
	}
}

func TestHoldoutStable(t *testing.T) {
	n := 0
	for i := range 1000 {
		sig := strings.Repeat("x", i%7) + string(rune('a'+i%26)) + strings.Repeat("y", i/26)
		if Holdout(sig, "", 0.1) != Holdout(sig, "", 0.1) {
			t.Fatal("not stable")
		}
		if Holdout(sig, "", 0.1) {
			n++
		}
	}
	if n < 50 || n > 150 {
		t.Errorf("holdout share %d/1000", n)
	}
	if Holdout("a", "", 0) {
		t.Error("frac 0 held out")
	}
}

func TestTrainingScrub(t *testing.T) {
	in := "user bob@example.com from 203.0.113.9 used sk_live_abcdefgh12345678 and AKIAABCDEFGHIJKLMNOP token=hunter2 on /a?id=42 with ghp_abcdefghijklmnopqrstuvwxyz0123"
	got := TrainingScrub(in)
	for _, leak := range []string{"bob@example.com", "203.0.113.9", "sk_live_", "AKIA", "hunter2", "id=42", "ghp_"} {
		if strings.Contains(got, leak) {
			t.Errorf("%q leaked: %s", leak, got)
		}
	}
	if !strings.Contains(got, "/a?id=") {
		t.Errorf("over-scrubbed: %s", got)
	}
}

// TestExportCatalogBootstrap writes the catalog test cases as SFT bootstrap
// data (they cover every extended catalog entry):
// FIX_BOOTSTRAP_OUT=training/data/catalog_tests.jsonl go test ./internal/ai -run ExportCatalogBootstrap
func TestExportCatalogBootstrap(t *testing.T) {
	out := os.Getenv("FIX_BOOTSTRAP_OUT")
	if out == "" {
		t.Skip("FIX_BOOTSTRAP_OUT not set")
	}
	agents := []string{"lovable", "bolt", "cursor", "replit", "v0", "other"}
	var list []SFTExample
	for i, tc := range catalogMoreCases() {
		c := *tc.c
		c.Agent = agents[i%len(agents)]
		c.Signature = tc.key + "/" + tc.name // stable holdout key
		if ex, ok := SFTFromContext(&c, "catalog_tests"); ok {
			list = append(list, ex)
		}
	}
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := WriteJSONL(f, list); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d examples -> %s", len(list), out)
}

package ai_test

// Runs only with FIX_TEST_DATABASE_URL (the schema is wiped).

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yourname/fixassist/internal/ai"
	"github.com/yourname/fixassist/internal/store/db"
	"github.com/yourname/fixassist/internal/verify"
)

const dbModelReply = `{"severity":"high","title":"Sign-up fails on the flux check","what_broke":"New people can't sign up.","why":"The profiles insert breaks the flux rule.","fix_prompt":"On /signup, POST /rest/v1/profiles fails with ZZ999 \"flux rule violated\". Find the flux rule on the profiles table and make the insert satisfy it. Change only that.","how_to_check":"Sign up with a new account."}`

type stubLLM struct {
	text  string
	err   error
	calls int
}

func (s *stubLLM) Complete(context.Context, ai.Request) (ai.Response, error) {
	s.calls++
	return ai.Response{Text: s.text, Model: "stub:1", TokensIn: 100, TokensOut: 50, Latency: 1500 * time.Millisecond}, s.err
}

func TestModelSuggestionAndTrainingRow(t *testing.T) {
	pool := kbSetup(t)
	ctx := context.Background()
	q := db.New(pool)
	pid := newProject(t, pool, "acme", "lovable")
	issue := netIssue(t, pool, pid, "lovable", "ZZ999", "flux rule violated")

	m := &stubLLM{text: dbModelReply}
	id, s, err := (&ai.Analyzer{Model: m}).Analyze(ctx, pool, issue, 1)
	if err != nil {
		t.Fatal(err)
	}
	if s.Origin != "model" || m.calls != 1 {
		t.Fatalf("origin %s, calls %d", s.Origin, m.calls)
	}
	var model, pv string
	var latency int32
	if err := pool.QueryRow(ctx, `select model, prompt_version, latency_ms from fix_suggestions where id = $1`, id).Scan(&model, &pv, &latency); err != nil {
		t.Fatal(err)
	}
	if model != "stub:1" || pv != ai.ModelPromptVersion || latency != 1500 {
		t.Errorf("stored model=%s pv=%s latency=%d", model, pv, latency)
	}

	// The training row: scrubbed evidence in, the model's own answer out
	// (without who-is-affected and the other data-driven additions).
	rows, err := q.TrainingExamples(ctx, db.TrainingExamplesParams{Since: ts(time.Time{})})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows %d, err %v", len(rows), err)
	}
	r := rows[0]
	if r.SuggestionID != id || r.Origin != "model" || r.Model.String != "stub:1" || !strings.Contains(r.Input, "flux rule violated") {
		t.Errorf("row %+v", r)
	}
	if strings.Contains(string(r.Output), "hit this") {
		t.Errorf("output has who-is-affected text: %s", r.Output)
	}

	// The agent reports its fix, then it's confirmed.
	if _, err := verify.ReportFix(ctx, pool, pid, issue, verify.Report{Source: "agent", Summary: "Made the insert set flux.", Files: []string{"src/signup.ts"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	answer(t, pool, issue, verify.Worked)
	rows, _ = q.TrainingExamples(ctx, db.TrainingExamplesParams{Since: ts(time.Time{})})
	if r := rows[0]; r.Outcome.String != "worked" || r.AgentSummary != "Made the insert set flux." || len(r.Files) != 1 {
		t.Errorf("after outcome: %+v", r)
	}
	if sft := ai.SFTFromRows(rows); len(sft) != 1 {
		t.Errorf("sft %d", len(sft))
	}
}

func TestModelDownFallsBackToGeneric(t *testing.T) {
	pool := kbSetup(t)
	pid := newProject(t, pool, "acme", "lovable")
	issue := netIssue(t, pool, pid, "lovable", "ZZ998", "odd failure")
	_, s, err := (&ai.Analyzer{Model: &stubLLM{err: errors.New("connection refused")}}).Analyze(context.Background(), pool, issue, 1)
	if err != nil || s.Origin != "agent" || s.FixPrompt == "" {
		t.Fatalf("origin %s err %v", s.Origin, err)
	}
}

func TestCatalogHitSkipsModel(t *testing.T) {
	pool := kbSetup(t)
	pid := newProject(t, pool, "acme", "lovable")
	issue := netIssue(t, pool, pid, "lovable", "23505", `duplicate key value violates unique constraint "profiles_pkey"`)
	m := &stubLLM{text: dbModelReply}
	_, s, err := (&ai.Analyzer{Model: m}).Analyze(context.Background(), pool, issue, 1)
	if err != nil || s.Origin != "catalog" || m.calls != 0 {
		t.Fatalf("origin %s calls %d err %v", s.Origin, m.calls, err)
	}
}

func TestTrainingOptOut(t *testing.T) {
	pool := kbSetup(t)
	ctx := context.Background()
	q := db.New(pool)
	pid := newProject(t, pool, "acme", "lovable")
	first := netIssue(t, pool, pid, "lovable", "ZZ997", "first")
	if _, _, err := ai.Analyze(ctx, pool, first, 1); err != nil {
		t.Fatal(err)
	}
	if err := q.SetProjectTrainingOptOut(ctx, db.SetProjectTrainingOptOutParams{ID: pid, OptOut: true}); err != nil {
		t.Fatal(err)
	}
	if err := q.DeleteProjectTraining(ctx, pid); err != nil {
		t.Fatal(err)
	}
	second := netIssue(t, pool, pid, "lovable", "ZZ996", "second")
	if _, _, err := ai.Analyze(ctx, pool, second, 1); err != nil {
		t.Fatal(err)
	}
	c, err := q.TrainingCounts(ctx)
	if err != nil || c.Total != 0 {
		t.Fatalf("opted-out project has %d training rows (err %v)", c.Total, err)
	}
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

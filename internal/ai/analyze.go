package ai

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/fixassist/internal/grouping"
	"github.com/yourname/fixassist/internal/store/db"
)

// Attempt numbers worked out from the issue's history (Load):
// AttemptAuto is one more than the fixes that didn't hold;
// AttemptAfterFailure is the same but at least 2 (a fix just failed).
const (
	AttemptAuto         = 0
	AttemptAfterFailure = -1
)

// Load builds the context for an issue from the database.
func Load(ctx context.Context, q *db.Queries, issueID int64, attempt int) (*Context, error) {
	is, err := q.GetIssueForAnalysis(ctx, issueID)
	if err != nil {
		return nil, err
	}
	st, err := q.IssueRecentStats(ctx, db.IssueRecentStatsParams{IssueID: issueID, Hour: pgtype.Timestamptz{Time: Now().Add(-24 * time.Hour), Valid: true}})
	if err != nil {
		return nil, err
	}
	samples, err := q.IssueSamples(ctx, db.IssueSamplesParams{IssueID: issueID, Limit: MaxSamples})
	if err != nil {
		return nil, err
	}
	c := &Context{
		ProjectID: is.ProjectID, IssueID: is.ID, Kind: is.Kind, Title: is.Title, Signature: is.Signature, Route: is.Route.String,
		Agent: is.TargetAgent, Backend: is.Backend, KeyPage: grouping.RouteWeight(is.Route.String, is.KeyPages) > 1,
		Users: is.UserCount, Events: is.EventCount, Events24h: st, FirstSeen: is.FirstSeenAt.Time,
		FirstRelease: is.FirstReleaseID.String, Regressions: is.RegressionCount, Attempt: attempt,
	}
	for _, s := range samples {
		c.AddSample(s.Payload)
	}
	// What was already tried (agent loop, "I applied the fix"), so the next
	// prompt doesn't repeat it.
	attempts, err := q.FixAttempts(ctx, issueID)
	if err != nil {
		return nil, err
	}
	failed := 0
	for _, a := range attempts {
		if a.Outcome.String == "failed" {
			failed++
			c.Previous = append(c.Previous, PrevAttempt{N: int(a.Attempt), Source: a.Source, Summary: a.Summary, Files: a.Files, Release: a.ReleaseID.String})
		}
	}
	// Automatic attempt numbers: one more than the fixes that didn't hold.
	switch c.Attempt {
	case AttemptAuto:
		c.Attempt = max(int(is.RegressionCount), failed) + 1
	case AttemptAfterFailure:
		c.Attempt = max(int(is.RegressionCount), failed, 1) + 1
	}
	if c.Attempt < 1 {
		c.Attempt = 1
	}
	return c, nil
}

func numeric(f float64) pgtype.Numeric {
	var n pgtype.Numeric
	_ = n.Scan(strconv.FormatFloat(f, 'f', 3, 64))
	return n
}

// Analyze explains one issue and stores the suggestion. It is the body of
// the analyze_issue job and of the "Explain now" button.
func Analyze(ctx context.Context, pool *pgxpool.Pool, issueID int64, attempt int) (int64, Suggestion, error) {
	return AnalyzeWith(ctx, pool, issueID, attempt, nil)
}

// AnalyzeWith also looks up code references (the GitHub App) when a
// searcher is given. The lookup happens before the transaction, and its
// failure never blocks the explanation.
func AnalyzeWith(ctx context.Context, pool *pgxpool.Pool, issueID int64, attempt int, code CodeSearcher) (int64, Suggestion, error) {
	return (&Analyzer{Code: code}).Analyze(ctx, pool, issueID, attempt)
}

// Analyzer holds the optional helpers of an analysis: code search, and our
// own model for errors outside the catalog. Both run before the
// transaction, and neither failing blocks the explanation.
type Analyzer struct {
	Code  CodeSearcher
	Model LLM
	Log   *slog.Logger
}

// Analyze explains one issue and stores the suggestion.
func (a *Analyzer) Analyze(ctx context.Context, pool *pgxpool.Pool, issueID int64, attempt int) (int64, Suggestion, error) {
	var id int64
	// Slowdowns have no events to analyze: every path that explains an
	// issue (new, came back, next attempt, re-explain) comes through here.
	if is, err := db.New(pool).GetIssueForAnalysis(ctx, issueID); err == nil && is.Kind == KindPerf {
		if attempt <= 0 { // AttemptAuto or AttemptAfterFailure: one more than the times it came back
			attempt = 1 + int(is.RegressionCount)
		}
		return explainPerf(ctx, pool, is, attempt)
	}
	c, err := Load(ctx, db.New(pool), issueID, attempt)
	if err != nil {
		return 0, Suggestion{}, err
	}
	s := Explain(c)
	if a.Code != nil {
		if terms := CodeTerms(c, s.CatalogKey); len(terms) > 0 {
			sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			refs, err := a.Code.Refs(sctx, c.ProjectID, terms)
			cancel()
			if err == nil && len(refs) > 0 {
				c.CodeRefs = refs
				s = Explain(c)
			}
		}
	}
	var mr *ModelResult
	if a.Model != nil && s.Origin == "agent" {
		var merr error
		s, mr, merr = ExplainWithModel(ctx, c, a.Model)
		if merr != nil && a.Log != nil {
			a.Log.Warn("model explanation failed; using the generic prompt", "issue_id", issueID, "err", merr)
		}
	}
	promptVersion := PromptVersion
	core := explainCore(c) // the training target
	if mr != nil {
		promptVersion = ModelPromptVersion
		core = mr.Core
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)

		// Knowledge base (Phase 7): one entry per error pattern across all
		// apps, signature + template id only, never samples.
		tmpl := s.CatalogKey
		if tmpl == "" {
			tmpl = "agent.generic"
		}
		kb, err := q.UpsertKnowledgeEntry(ctx, db.UpsertKnowledgeEntryParams{
			Sig: c.Signature, ErrorType: c.Kind, Framework: c.Backend,
			CatalogKey:          pgtype.Text{String: s.CatalogKey, Valid: s.CatalogKey != ""},
			ExplanationTemplate: PromptVersion + ":" + tmpl, PromptsByPlatform: []byte("{}"),
		})
		if err != nil {
			return err
		}
		if err := q.AddKBSighting(ctx, db.AddKBSightingParams{EntryID: kb.ID, ProjectHash: projectHash(c.ProjectID)}); err != nil {
			return err
		}
		kbID := pgtype.Int8{Int64: kb.ID, Valid: true}
		switch {
		case kb.QuarantinedAt.Valid && s.Origin == "catalog":
			// The standard fix keeps failing for this pattern: hand the
			// diagnosis to the founder's agent instead, and don't count the
			// answer against the quarantined pattern.
			s = QuarantineFallback(c)
			core = s
			s.FixPrompt += whereToLook(c.CodeRefs)
			kbID = pgtype.Int8{}
		case !kb.QuarantinedAt.Valid && kb.Worked >= ProvenMinWorked && kb.Confidence >= ProvenMinConfidence:
			s.Origin = "cache" // reused: this pattern's fix is proven across apps
		}

		id, err = q.InsertSuggestion(ctx, db.InsertSuggestionParams{
			IssueID: issueID, Origin: s.Origin, CatalogKey: pgtype.Text{String: s.CatalogKey, Valid: s.CatalogKey != ""},
			PromptVersion: promptVersion, TargetAgent: c.Agent, Severity: s.Severity, Title: s.Title,
			WhatBroke: s.WhatBroke, WhoIsAffected: s.WhoIsAffected, Why: s.Why, FixPrompt: s.FixPrompt,
			HowToCheck: s.HowToCheck, Confidence: numeric(s.Confidence), Attempt: int32(c.Attempt),
			KbEntryID: kbID, CodeRefs: codeRefsJSON(c.CodeRefs),
		})
		if err != nil {
			return err
		}
		var model pgtype.Text
		if mr != nil && s.Origin != "catalog" {
			model = pgtype.Text{String: mr.Model, Valid: true}
			if err := q.SetSuggestionModel(ctx, db.SetSuggestionModelParams{
				ID: id, Model: model, TokensIn: int32(mr.TokensIn), TokensOut: int32(mr.TokensOut),
				CostUsd: numeric(mr.CostUSD), LatencyMs: int32(mr.Latency.Milliseconds()),
			}); err != nil {
				return err
			}
		}
		// Fine-tuning data (skipped in SQL when the project opted out).
		if err := q.InsertTrainingExample(ctx, db.InsertTrainingExampleParams{
			ProjectID: c.ProjectID, IssueID: issueID, SuggestionID: id, Signature: c.Signature, Origin: s.Origin,
			CatalogKey: pgtype.Text{String: s.CatalogKey, Valid: s.CatalogKey != ""}, Model: model,
			PromptVersion: promptVersion, Attempt: int32(c.Attempt), Input: TrainingInput(c), Output: TrainingOutput(core),
		}); err != nil {
			return err
		}
		return q.MarkIssueExplained(ctx, issueID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, s, err
	}
	return id, s, err
}

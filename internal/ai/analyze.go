package ai

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/fixassist/internal/grouping"
	"github.com/yourname/fixassist/internal/store/db"
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
	if c.Attempt < 1 {
		c.Attempt = 1
	}
	for _, s := range samples {
		c.AddSample(s.Payload)
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
	var id int64
	c, err := Load(ctx, db.New(pool), issueID, attempt)
	if err != nil {
		return 0, Suggestion{}, err
	}
	s := Explain(c)
	if code != nil {
		if terms := CodeTerms(c, s.CatalogKey); len(terms) > 0 {
			sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			refs, err := code.Refs(sctx, c.ProjectID, terms)
			cancel()
			if err == nil && len(refs) > 0 {
				c.CodeRefs = refs
				s = Explain(c)
			}
		}
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
			s.FixPrompt += whereToLook(c.CodeRefs)
			kbID = pgtype.Int8{}
		case !kb.QuarantinedAt.Valid && kb.Worked >= ProvenMinWorked && kb.Confidence >= ProvenMinConfidence:
			s.Origin = "cache" // reused: this pattern's fix is proven across apps
		}

		id, err = q.InsertSuggestion(ctx, db.InsertSuggestionParams{
			IssueID: issueID, Origin: s.Origin, CatalogKey: pgtype.Text{String: s.CatalogKey, Valid: s.CatalogKey != ""},
			PromptVersion: PromptVersion, TargetAgent: c.Agent, Severity: s.Severity, Title: s.Title,
			WhatBroke: s.WhatBroke, WhoIsAffected: s.WhoIsAffected, Why: s.Why, FixPrompt: s.FixPrompt,
			HowToCheck: s.HowToCheck, Confidence: numeric(s.Confidence), Attempt: int32(c.Attempt),
			KbEntryID: kbID, CodeRefs: codeRefsJSON(c.CodeRefs),
		})
		if err != nil {
			return err
		}
		return q.MarkIssueExplained(ctx, issueID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, s, err
	}
	return id, s, err
}

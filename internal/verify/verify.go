// Package verify closes the loop: explicit "did it work?" answers and
// implicit verification after the next release (FDR "Fix feedback and
// verification").
//
// States: new → explained → verifying → fixed, back to open on recurrence.
//
//  1. explained → verifying when a release first seen after the suggestion
//     appears, or when the founder clicks "I applied the fix".
//  2. verifying → fixed when, on that release, the issue's page has ≥ N
//     sessions, ≥ 48 h have passed and the fingerprint has 0 events
//     (writes worked/implicit).
//  3. verifying/fixed → open when the fingerprint appears on a release at or
//     after the one being watched (grouping does this; writes failed/implicit).
//  4. Low traffic: after 7 days without N sessions, "probably fixed?" is
//     shown and the founder decides; nothing implicit is written.
//  5. Explicit answers always win.
package verify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/fixassist/internal/store/db"
)

// Rules are the FDR starting values; tune them on design-partner data.
type Rules struct {
	MinSessions     int64
	MinAge          time.Duration
	LowTrafficAfter time.Duration
}

// DefaultRules: N = 20 sessions, 48 h, 7 days.
var DefaultRules = Rules{MinSessions: 20, MinAge: 48 * time.Hour, LowTrafficAfter: 7 * 24 * time.Hour}

// Quarantine (Phase 7): a pattern whose fix has at least this many answers
// and confidence below QuarantineBelow stops being offered.
const (
	QuarantineMinAnswers = 5
	QuarantineBelow      = "0.3"
)

// Outcomes and sources.
const (
	Worked   = "worked"
	Failed   = "failed"
	Partial  = "partial"
	Explicit = "explicit"
	Implicit = "implicit"
)

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
func text(s string) pgtype.Text         { return pgtype.Text{String: s, Valid: s != ""} }

// Record writes one feedback row for the issue's latest suggestion and
// updates the knowledge entry's counts and confidence. Use inside a tx.
func Record(ctx context.Context, q *db.Queries, issueID int64, outcome, source, release string, userID int64) error {
	sugID, kbID := pgtype.Int8{}, pgtype.Int8{}
	if s, err := q.LatestSuggestion(ctx, issueID); err == nil {
		sugID, kbID = pgtype.Int8{Int64: s.ID, Valid: true}, s.KbEntryID
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	uid := pgtype.Int8{Int64: userID, Valid: userID != 0}
	if _, err := q.InsertFeedback(ctx, db.InsertFeedbackParams{
		SuggestionID: sugID, IssueID: issueID, Outcome: outcome, Source: source, ReleaseID: text(release), UserID: uid,
	}); err != nil {
		return err
	}
	if sugID.Valid { // the label for fine-tuning
		if err := q.SetTrainingOutcome(ctx, db.SetTrainingOutcomeParams{Outcome: text(outcome), OutcomeSource: text(source), SuggestionID: sugID.Int64}); err != nil {
			return err
		}
	}
	var w, f int32
	switch outcome {
	case Worked:
		w = 1
	case Failed:
		f = 1
	default:
		return nil // partial doesn't move confidence
	}
	// The applied fix (agent loop) now has its outcome.
	if err := q.CloseLatestAttempt(ctx, db.CloseLatestAttemptParams{IssueID: issueID, Outcome: text(outcome)}); err != nil {
		return err
	}
	if !kbID.Valid {
		return nil // a quarantine fallback: the answer isn't about the pattern's fix
	}
	var below pgtype.Numeric
	_ = below.Scan(QuarantineBelow)
	return q.UpdateKnowledgeFromFeedback(ctx, db.UpdateKnowledgeFromFeedbackParams{
		ID: kbID.Int64, Column2: w, Column3: f, Column4: QuarantineMinAnswers, Column5: below,
	})
}

// Answer records an explicit answer and sets the state directly:
// worked → fixed (watching newer releases), failed → open, partial → verifying.
func Answer(ctx context.Context, pool *pgxpool.Pool, projectID, issueID int64, outcome string, userID int64, now time.Time) error {
	if outcome != Worked && outcome != Failed && outcome != Partial {
		return fmt.Errorf("bad outcome %q", outcome)
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		kind, err := q.IssueKind(ctx, issueID)
		if err != nil {
			return err
		}
		release, since := "", now
		if r, err := q.LatestRelease(ctx, db.LatestReleaseParams{ProjectID: projectID, Server: kind == KindServer}); err == nil {
			release, since = r.ReleaseID, r.FirstSeenAt.Time
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		switch outcome {
		case Worked:
			// Fixed in the current release: the same release seeing it again
			// (old tabs) doesn't reopen it; any newer release does.
			if _, err := q.SetIssueFixedExplicit(ctx, db.SetIssueFixedExplicitParams{ID: issueID, ReleaseID: text(release), WatchSince: ts(since)}); err != nil {
				return err
			}
		case Failed:
			if _, err := q.SetIssueOpen(ctx, issueID); err != nil {
				return err
			}
		case Partial:
			if _, err := q.StartVerifyingManual(ctx, db.StartVerifyingManualParams{ID: issueID, ProjectID: projectID, WatchSince: ts(now)}); err != nil {
				return err
			}
		}
		return Record(ctx, q, issueID, outcome, Explicit, release, userID)
	})
}

// Applied is the "I applied the fix" button: watch the next release. It is
// recorded as the founder's attempt, like an agent's report_fix.
func Applied(ctx context.Context, pool *pgxpool.Pool, projectID, issueID int64, now time.Time) error {
	_, err := ReportFix(ctx, pool, projectID, issueID, Report{Source: "founder"}, now)
	if errors.Is(err, ErrTooManyAttempts) {
		// The founder can always keep trying; only agents are stopped.
		_, err = db.New(pool).StartVerifyingManual(ctx, db.StartVerifyingManualParams{ID: issueID, ProjectID: projectID, WatchSince: ts(now)})
	}
	if errors.Is(err, ErrNotFixable) {
		return nil
	}
	return err
}

// Stats says what one verification pass did.
type Stats struct {
	StartedVerifying int
	VerifiedFixed    int
	ProbablyFixed    int
}

// KindServer is ingest.KindServer (not imported: ingest doesn't depend on verify, but grouping does).
const KindServer = "server"

// Run applies rules 1, 2 and 4 to every explained or verifying issue. It is
// the verify_fixes job (every 15 minutes). State changes are conditional on
// the current status, so overlapping runs are safe.
//
// Server errors differ in two ways. Their releases are the server's own
// (stored as "server:<release>"), so only a server deploy starts their
// verification. And page traffic carries the frontend's release, so their
// traffic and recurrences are counted from the time the watch started; after
// "I applied the fix" they don't wait for a release name to change, since
// many servers keep the same one across deploys.
func Run(ctx context.Context, pool *pgxpool.Pool, rules Rules, now time.Time) (Stats, error) {
	var st Stats
	q := db.New(pool)
	list, err := q.IssuesToVerify(ctx)
	if err != nil {
		return st, err
	}
	for _, is := range list {
		server := is.Kind == KindServer
		switch is.Status {
		case "explained":
			r, err := q.FirstReleaseSince(ctx, db.FirstReleaseSinceParams{ProjectID: is.ProjectID, FirstSeenAt: is.SuggestedAt, Server: server})
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return st, err
			}
			n, err := q.StartVerifying(ctx, db.StartVerifyingParams{ID: is.ID, ReleaseID: text(r.ReleaseID), WatchSince: r.FirstSeenAt})
			if err != nil {
				return st, err
			}
			st.StartedVerifying += int(n)

		case "verifying":
			release, since := is.VerifyReleaseID.String, is.WatchSince
			if !is.VerifyReleaseID.Valid && !server {
				// "I applied the fix": wait for the next release.
				r, err := q.FirstReleaseSince(ctx, db.FirstReleaseSinceParams{ProjectID: is.ProjectID, FirstSeenAt: is.WatchSince})
				if errors.Is(err, pgx.ErrNoRows) {
					continue
				}
				if err != nil {
					return st, err
				}
				if err := q.SetVerifyRelease(ctx, db.SetVerifyReleaseParams{ID: is.ID, ReleaseID: text(r.ReleaseID), WatchSince: r.FirstSeenAt}); err != nil {
					return st, err
				}
				release, since = r.ReleaseID, r.FirstSeenAt
			}
			var events, sessions int64
			if server {
				events, err = q.EventsSince(ctx, db.EventsSinceParams{ProjectID: is.ProjectID, Fingerprint: is.Fingerprint, Since: since})
			} else {
				events, err = q.EventsOnRelease(ctx, db.EventsOnReleaseParams{ProjectID: is.ProjectID, Fingerprint: is.Fingerprint, ReleaseID: text(release)})
			}
			if err != nil {
				return st, err
			}
			if events > 0 {
				continue // grouping reopens it (rule 3)
			}
			if server {
				sessions, err = q.SessionsOnRouteSince(ctx, db.SessionsOnRouteSinceParams{ProjectID: is.ProjectID, Route: is.Route.String, Since: since})
			} else {
				sessions, err = q.SessionsOnRoute(ctx, db.SessionsOnRouteParams{ProjectID: is.ProjectID, Route: is.Route.String, ReleaseID: text(release)})
			}
			if err != nil {
				return st, err
			}
			age := now.Sub(since.Time)
			switch {
			case sessions >= rules.MinSessions && age >= rules.MinAge:
				err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
					tq := db.New(tx)
					n, err := tq.MarkVerifiedFixed(ctx, is.ID)
					if err != nil || n == 0 {
						return err
					}
					st.VerifiedFixed++
					return Record(ctx, tq, is.ID, Worked, Implicit, release, 0)
				})
				if err != nil {
					return st, err
				}
			case age >= rules.LowTrafficAfter && sessions < rules.MinSessions && !is.ProbablyFixedAt.Valid:
				if err := q.SetProbablyFixed(ctx, is.ID); err != nil {
					return st, err
				}
				st.ProbablyFixed++
			}
		}
	}
	return st, nil
}

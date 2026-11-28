package verify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/fixassist/internal/store/db"
)

// The agent loop: a fix is reported, then checked quickly, attempt after
// attempt. The quick check answers within minutes so a coding agent can
// iterate; the slower rules in Run (20 sessions, 48 h) still decide when an
// issue is finally "fixed".

// MaxAttempts is how many fixes may fail before a person has to step in.
const MaxAttempts = 3

// Quick-check thresholds: enough real use of the page, or the agent
// repeating the failing action itself and nothing being reported.
const (
	QuickMinSessions = 3
	QuickReproWait   = 30 * time.Second
)

// Quick-check states.
const (
	StateNotReported = "not_reported"       // no fix reported yet
	StateWaitDeploy  = "waiting_for_deploy" // the fixed version hasn't been seen yet
	StateWatching    = "watching"           // deployed, no repeats, not enough use yet
	StateCameBack    = "came_back"          // the error happened again on the fixed version
	StateLooksFixed  = "looks_fixed"        // used (or reproduced) since the fix, no repeats
	StateFixed       = "fixed"              // verified by the full rules or by the founder
	StateNeedsHuman  = "needs_human"        // MaxAttempts fixes failed
)

var (
	ErrTooManyAttempts = errors.New("this issue's fix failed too many times; a person has to look at it")
	ErrNotFixable      = errors.New("this issue is fixed or ignored")
)

// Report is one applied fix.
type Report struct {
	Source  string // agent or founder
	TokenID int64  // the agent token, when Source is agent
	Release string // optional: the version that contains the fix
	Summary string // what was changed
	Files   []string
}

// failedAttempts counts attempts whose fix didn't hold.
func failedAttempts(list []db.FixAttempt) int {
	n := 0
	for _, a := range list {
		if a.Outcome.String == Failed {
			n++
		}
	}
	return n
}

// ReportFix records an applied fix and starts watching for it: the issue
// becomes "verifying" from now. It returns the attempt number.
func ReportFix(ctx context.Context, pool *pgxpool.Pool, projectID, issueID int64, r Report, now time.Time) (int, error) {
	attempt := 0
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		prev, err := q.FixAttempts(ctx, issueID)
		if err != nil {
			return err
		}
		if failedAttempts(prev) >= MaxAttempts {
			return ErrTooManyAttempts
		}
		// A new report replaces a pending one (reported twice before deploying).
		n, err := q.StartVerifyingManual(ctx, db.StartVerifyingManualParams{ID: issueID, ProjectID: projectID, WatchSince: ts(now)})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFixable
		}
		sug := pgtype.Int8{}
		if s, err := q.LatestSuggestion(ctx, issueID); err == nil {
			sug = pgtype.Int8{Int64: s.ID, Valid: true}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		attempt = len(prev) + 1
		files := r.Files
		if files == nil {
			files = []string{}
		}
		_, err = q.InsertFixAttempt(ctx, db.InsertFixAttemptParams{
			IssueID: issueID, SuggestionID: sug, Attempt: int32(attempt), Source: r.Source,
			AgentTokenID: pgtype.Int8{Int64: r.TokenID, Valid: r.TokenID != 0},
			ReleaseID:    text(strings.TrimSpace(r.Release)), Summary: strings.TrimSpace(r.Summary), Files: files,
			ReportedAt: ts(now),
		})
		if err != nil || !sug.Valid {
			return err
		}
		// What the agent changed, next to the prompt it was given.
		return q.SetTrainingAttempt(ctx, db.SetTrainingAttemptParams{AgentSummary: strings.TrimSpace(r.Summary), Files: files, SuggestionID: sug.Int64})
	})
	return attempt, err
}

// Quick is the answer to "did my fix work?".
type Quick struct {
	State       string
	Attempt     int // the latest attempt's number (0: none)
	Failed      int // attempts that didn't hold
	Release     string
	Since       time.Time // when watching started
	Minutes     int
	Sessions    int64 // sessions on the issue's page since then
	Occurrences int64 // the error since then
	LastSeen    time.Time
	Message     string // what it means and what to do next, for the agent
}

// QuickCheck reports where the latest fix stands. reproduced marks that the
// caller just repeated the failing action; QuickReproWait later, no new
// occurrence means it looks fixed even without other traffic.
func QuickCheck(ctx context.Context, pool *pgxpool.Pool, is db.Issue, reproduced bool, now time.Time) (Quick, error) {
	q := db.New(pool)
	list, err := q.FixAttempts(ctx, is.ID)
	if err != nil {
		return Quick{}, err
	}
	out := Quick{Failed: failedAttempts(list)}
	if len(list) == 0 {
		out.State, out.Message = StateNotReported, "No fix has been reported for this issue yet. Apply the fix from get_issue, deploy it, then call report_fix."
		return out, nil
	}
	last := list[len(list)-1]
	out.Attempt = int(last.Attempt)
	switch {
	case is.Status == "fixed":
		out.State, out.Message = StateFixed, "Verified fixed. Nothing more to do for this issue."
		return out, nil
	case last.Outcome.String == Failed && out.Failed >= MaxAttempts:
		out.State = StateNeedsHuman
		out.Message = fmt.Sprintf("%d fixes didn't hold. Stop and tell the founder: this issue needs a person to look at it.", out.Failed)
		return out, nil
	case last.Outcome.String == Failed:
		out.State = StateCameBack
		out.Message = fmt.Sprintf("The error came back after attempt %d. Call get_issue for the new evidence and the attempt %d prompt, then try again.", out.Attempt, out.Attempt+1)
		return out, nil
	}
	if is.Kind == KindPerf {
		// A slowdown has no error to repeat: speed is measured in visitors'
		// browsers on the new version, which takes real visits and time.
		out.State = StateWatching
		out.Message = "This is a speed issue: Fix Assistant measures the new version in real visitors' browsers and marks it fixed once the page is back to its earlier speed. That needs a deploy and some visits (usually a day). Nothing more to do now; check again tomorrow."
		return out, nil
	}
	if reproduced && !last.ReproducedAt.Valid {
		if err := q.MarkAttemptReproduced(ctx, db.MarkAttemptReproducedParams{ID: last.ID, At: ts(now)}); err != nil {
			return out, err
		}
		last.ReproducedAt = ts(now)
	}

	// Which version holds the fix, and since when to watch.
	server := is.Kind == KindServer
	since := last.ReportedAt.Time
	release := last.ReleaseID.String
	if server && release != "" && !strings.HasPrefix(release, "server:") {
		release = "server:" + release
	}
	switch {
	case release != "":
		first, err := q.ReleaseFirstSeen(ctx, db.ReleaseFirstSeenParams{ProjectID: is.ProjectID, ReleaseID: release})
		switch {
		case errors.Is(err, pgx.ErrNoRows) && server:
			// A server's version is only seen when it reports an error,
			// and a working fix reports none: watch from the report.
			release = ""
		case errors.Is(err, pgx.ErrNoRows):
			out.State, out.Release = StateWaitDeploy, release
			out.Message = fmt.Sprintf("Version %s hasn't been seen yet. Make sure it is published, open the site once, then check again.", release)
			return out, nil
		case err != nil:
			return out, err
		case first.Time.Before(since):
			since = first.Time // deployed before the report
		}
	case !server:
		// Browser versions are named by the SDK: the next one seen after the report.
		r, err := q.FirstReleaseSince(ctx, db.FirstReleaseSinceParams{ProjectID: is.ProjectID, FirstSeenAt: ts(since), Server: false})
		if errors.Is(err, pgx.ErrNoRows) {
			out.State = StateWaitDeploy
			out.Message = "No new version of the site has been seen since the fix was reported. Publish it, open the site once, then check again."
			return out, nil
		}
		if err != nil {
			return out, err
		}
		release, since = r.ReleaseID, r.FirstSeenAt.Time
	}
	// Servers that keep one release name: watch from the report.
	out.Release, out.Since = release, since
	out.Minutes = int(now.Sub(since).Minutes())

	if release != "" && !server {
		out.Occurrences, err = q.EventsOnRelease(ctx, db.EventsOnReleaseParams{ProjectID: is.ProjectID, Fingerprint: is.Fingerprint, ReleaseID: text(release)})
		if err == nil {
			out.Sessions, err = q.SessionsOnRoute(ctx, db.SessionsOnRouteParams{ProjectID: is.ProjectID, Route: is.Route.String, ReleaseID: text(release)})
		}
	} else {
		out.Occurrences, err = q.EventsSince(ctx, db.EventsSinceParams{ProjectID: is.ProjectID, Fingerprint: is.Fingerprint, Since: ts(since)})
		if err == nil {
			out.Sessions, err = q.SessionsOnRouteSince(ctx, db.SessionsOnRouteSinceParams{ProjectID: is.ProjectID, Route: is.Route.String, Since: ts(since)})
		}
	}
	if err != nil {
		return out, err
	}
	if out.Occurrences > 0 {
		if t, err := q.LastEventSince(ctx, db.LastEventSinceParams{ProjectID: is.ProjectID, Fingerprint: is.Fingerprint, Since: ts(since)}); err == nil {
			out.LastSeen = t.Time
		}
		out.State = StateCameBack
		out.Message = fmt.Sprintf("The error happened %d more time(s) on the fixed version. Call get_issue for the new evidence and the attempt %d prompt (it can take a few seconds to be written), then try again.", out.Occurrences, out.Attempt+1)
		return out, nil
	}
	page := is.Route.String
	if page == "" {
		page = "the failing action"
	}
	reproOK := last.ReproducedAt.Valid && now.Sub(last.ReproducedAt.Time) >= QuickReproWait && !last.ReproducedAt.Time.Before(since)
	switch {
	case out.Sessions >= QuickMinSessions || reproOK:
		out.State = StateLooksFixed
		out.Message = fmt.Sprintf("No repeats since the fix (%d sessions on %s, %d min). It looks fixed. Fix Assistant keeps watching and marks it fixed after 20 sessions and 48 hours; if it comes back, the founder is told.", out.Sessions, page, out.Minutes)
	case last.ReproducedAt.Valid && !last.ReproducedAt.Time.Before(since):
		out.State = StateWatching
		wait := QuickReproWait - now.Sub(last.ReproducedAt.Time)
		out.Message = fmt.Sprintf("Reproduction noted. Check again in %d seconds: if the error doesn't arrive by then, it looks fixed.", int(wait.Seconds())+1)
	default:
		out.State = StateWatching
		out.Message = fmt.Sprintf("Deployed and no repeats yet, but %s has had only %d session(s) since the fix. Repeat the action that failed (open %s and do what users did), then call check_fix with reproduced=true.", page, out.Sessions, page)
	}
	return out, nil
}

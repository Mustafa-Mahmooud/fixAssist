// Package autofix sends high-severity issues to the founder's own coding
// agent without them asking. When an issue is explained with high severity
// and the project has auto-fix on, a run is created: it waits for the
// founder's approval (or a rule approves it), then the founder's local
// daemon (cmd/fixassist) claims it, runs the agent in a separate git
// worktree, opens a pull request and, when allowed, turns on auto-merge.
// After the merge the fix enters the usual verification loop (verify).
//
// Rule-approved runs never merge on their own: the evidence in a fix prompt
// comes from the app's visitors, so a person approves the diff first.
package autofix

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/fixassist/internal/store/db"
	"github.com/yourname/fixassist/internal/verify"
)

// Limits.
const (
	Lease        = 60 * time.Minute // a claimed run fails if the daemon goes quiet this long
	OfflineAfter = 10 * time.Minute // no poll for this long: the daemon is offline
	ApprovalTTL  = 7 * 24 * time.Hour
)

// Rules.
const (
	RuleOff  = "off"
	RuleHigh = "high" // approve every high-severity issue (pull request only)
)

// Run states (see migration 00015).
const (
	PendingApproval       = "pending_approval"
	Approved              = "approved"
	Claimed               = "claimed"
	PROpened              = "pr_opened"
	AwaitingMergeApproval = "awaiting_merge_approval"
	MergeApproved         = "merge_approved"
	Merged                = "merged"
	Failed                = "failed"
	Rejected              = "rejected"
	Expired               = "expired"
	Emailed               = "emailed"
)

// Open says whether a run is still in progress.
func Open(state string) bool {
	switch state {
	case PendingApproval, Approved, Claimed, PROpened, AwaitingMergeApproval, MergeApproved:
		return true
	}
	return false
}

// ErrNotAllowed is a state change that doesn't apply to the run (any more).
var ErrNotAllowed = errors.New("this auto-fix can't do that now")

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func failedAttempts(list []db.FixAttempt) int {
	n := 0
	for _, a := range list {
		if a.Outcome.String == verify.Failed {
			n++
		}
	}
	return n
}

// fixable says whether an agent may work on the issue at all.
func fixable(ctx context.Context, q *db.Queries, issueID int64) (db.GetIssueForAnalysisRow, bool, error) {
	is, err := q.GetIssueForAnalysis(ctx, issueID)
	if err != nil {
		return is, false, err
	}
	if is.Status == "fixed" || is.Status == "ignored" {
		return is, false, nil
	}
	list, err := q.FixAttempts(ctx, issueID)
	if err != nil {
		return is, false, err
	}
	return is, failedAttempts(list) < verify.MaxAttempts, nil
}

// Consider is called after an issue is explained. With auto-fix on and a
// high-severity suggestion it creates a run: approved when the project's
// rule says so, otherwise waiting for the founder. created is false when
// nothing was created (off, not high, or a run is already in progress).
func Consider(ctx context.Context, pool *pgxpool.Pool, issueID int64, s db.FixSuggestion, now time.Time) (db.AutofixRun, bool, error) {
	if s.Severity != "high" {
		return db.AutofixRun{}, false, nil
	}
	q := db.New(pool)
	is, ok, err := fixable(ctx, q, issueID)
	if err != nil || !ok {
		return db.AutofixRun{}, false, err
	}
	set, err := q.ProjectAutofix(ctx, is.ProjectID)
	if err != nil || !set.AutofixEnabled {
		return db.AutofixRun{}, false, err
	}
	p := db.InsertAutofixRunParams{ProjectID: is.ProjectID, IssueID: is.ID, SuggestionID: pgtype.Int8{Int64: s.ID, Valid: true}, State: PendingApproval}
	if set.AutofixRule == RuleHigh {
		p.State = Approved
		p.ApprovedBy = pgtype.Text{String: "rule", Valid: true}
		p.ApprovedAt = ts(now)
	}
	return insert(ctx, q, p)
}

// MaxHint bounds the founder's note on a run.
const MaxHint = 1000

// Start is the founder's "Auto-fix this" on an issue page: an approved run
// for the latest suggestion, whatever its severity. hint is the founder's
// note for the agent ("it's in Controllers/LearningErrors.cs"), or "".
func Start(ctx context.Context, pool *pgxpool.Pool, projectID, issueID, userID int64, hint string, now time.Time) (db.AutofixRun, error) {
	hint = clip(strings.TrimSpace(hint), MaxHint)
	q := db.New(pool)
	is, ok, err := fixable(ctx, q, issueID)
	if err != nil {
		return db.AutofixRun{}, err
	}
	if !ok || is.ProjectID != projectID {
		return db.AutofixRun{}, ErrNotAllowed
	}
	if r, err := q.LatestAutofixRun(ctx, issueID); err == nil && r.State == PendingApproval {
		return q.ApproveAutofixRun(ctx, db.ApproveAutofixRunParams{ID: r.ID, ProjectID: projectID, UserID: user(userID), Hint: hint})
	}
	s, err := q.LatestSuggestion(ctx, issueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.AutofixRun{}, ErrNotAllowed
	}
	if err != nil {
		return db.AutofixRun{}, err
	}
	r, created, err := insert(ctx, q, db.InsertAutofixRunParams{ProjectID: projectID, IssueID: issueID,
		SuggestionID: pgtype.Int8{Int64: s.ID, Valid: true}, State: Approved,
		ApprovedBy: pgtype.Text{String: "user", Valid: true}, ApprovedUserID: user(userID), ApprovedAt: ts(now), Hint: hint})
	if err == nil && !created {
		return r, ErrNotAllowed // one is already in progress
	}
	return r, err
}

func insert(ctx context.Context, q *db.Queries, p db.InsertAutofixRunParams) (db.AutofixRun, bool, error) {
	r, err := q.InsertAutofixRun(ctx, p)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// user is a users.id, or NULL for 0 (approved from a signed email link).
func user(id int64) pgtype.Int8 { return pgtype.Int8{Int64: id, Valid: id != 0} }

// enabled says whether the project still has auto-fix on.
func enabled(ctx context.Context, q *db.Queries, projectID int64) (bool, error) {
	set, err := q.ProjectAutofix(ctx, projectID)
	return set.AutofixEnabled, err
}

// Why an approved run isn't being claimed, though a daemon is polling.
const (
	HeldOff  = "off"  // auto-fix was turned off after approving
	HeldBusy = "busy" // another run is in progress (one at a time)
)

// Held says which of Claim's gates holds the project's approved runs back,
// or "" when none does (the next poll takes one). Without it the dashboard
// says "waiting for the daemon" while the daemon is waiting for us.
func Held(ctx context.Context, q *db.Queries, projectID int64) (string, error) {
	if on, err := enabled(ctx, q, projectID); err != nil || !on {
		return HeldOff, err
	}
	running, err := q.ClaimedAutofixRuns(ctx, projectID)
	if err != nil || running == 0 {
		return "", err
	}
	return HeldBusy, nil
}

// Why a run failed, when it's known.
const FailNotInRepo = "not_in_repo" // the agent didn't find the code that raises the error

// NotInRepoTag is how the agent says so: the prompt asks it to end with
// "CANNOT_FIX: NOT_IN_REPO: <what it looked for>".
const NotInRepoTag = "NOT_IN_REPO"

// ValidBranch says whether name is safe to hand the daemon as the branch to
// fix from. It reaches git and gh on the founder's computer as an
// argument, so it's held to a strict form: no leading '-' (an option), no
// '..', '@{', spaces or control characters, and only ordinary characters.
func ValidBranch(name string) bool {
	if name == "" || len(name) > 200 || name[0] == '-' || name[0] == '/' || name[0] == '.' ||
		strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") || strings.HasSuffix(name, ".lock") ||
		strings.Contains(name, "..") || strings.Contains(name, "//") || strings.Contains(name, "/.") || strings.Contains(name, "@{") {
		return false
	}
	for _, c := range name {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._/-+", c)
		if !ok {
			return false
		}
	}
	return true
}

// SetBranch sets the branch auto-fix works from ("" = the daemon's own).
func SetBranch(ctx context.Context, pool *pgxpool.Pool, projectID int64, branch string) error {
	if branch = strings.TrimSpace(branch); branch != "" && !ValidBranch(branch) {
		return ErrBadBranch
	}
	return db.New(pool).SetProjectAutofixBranch(ctx, db.SetProjectAutofixBranchParams{ID: projectID, Branch: branch})
}

// ErrBadBranch is a branch name auto-fix won't use.
var ErrBadBranch = errors.New("that isn't a branch name auto-fix can use: letters, digits and . _ / - + only")

// Approve is the founder approving a waiting run (userID 0: from the
// signed email link).
func Approve(ctx context.Context, pool *pgxpool.Pool, projectID, runID, userID int64) (db.AutofixRun, error) {
	q := db.New(pool)
	if on, err := enabled(ctx, q, projectID); err != nil || !on {
		if err == nil {
			err = ErrNotAllowed
		}
		return db.AutofixRun{}, err
	}
	r, err := q.ApproveAutofixRun(ctx, db.ApproveAutofixRunParams{ID: runID, ProjectID: projectID, UserID: user(userID)})
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotAllowed
	}
	return r, err
}

// Reject cancels a run that hasn't started.
func Reject(ctx context.Context, pool *pgxpool.Pool, projectID, runID int64) error {
	_, err := db.New(pool).RejectAutofixRun(ctx, db.RejectAutofixRunParams{ID: runID, ProjectID: projectID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotAllowed
	}
	return err
}

// ApproveMerge is the founder approving a rule-approved pull request after
// reading the diff: the daemon then turns on auto-merge.
func ApproveMerge(ctx context.Context, pool *pgxpool.Pool, projectID, runID, userID int64) error {
	_, err := db.New(pool).ApproveAutofixMerge(ctx, db.ApproveAutofixMergeParams{ID: runID, ProjectID: projectID, UserID: user(userID)})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotAllowed
	}
	return err
}

// Task kinds a daemon is handed.
const (
	TaskFix   = "fix"   // write the fix and open a pull request
	TaskMerge = "merge" // turn on auto-merge for an approved pull request
)

// claimLock namespaces the per-project advisory lock for claims.
const claimLock int32 = 726_2015

// Claim hands the daemon its next task: a merge approval first (quick), then
// the oldest approved run, one at a time. ok is
// false when there is nothing to do.
func Claim(ctx context.Context, pool *pgxpool.Pool, projectID, tokenID int64, now time.Time) (kind string, run db.AutofixRun, ok bool, err error) {
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock($1, $2)", claimLock, int32(projectID)); err != nil {
			return err
		}
		r, err := q.NextMergeApprovedRun(ctx, projectID)
		if err == nil {
			kind, run, ok = TaskMerge, r, true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// Turning auto-fix off stops new fixes; approved runs wait.
		if on, err := enabled(ctx, q, projectID); err != nil || !on {
			return err
		}
		running, err := q.ClaimedAutofixRuns(ctx, projectID)
		if err != nil {
			return err
		}
		if running > 0 {
			return nil
		}
		for {
			next, err := q.NextApprovedAutofixRun(ctx, projectID)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			// The issue may have been fixed, ignored or given up on since.
			if _, fix, err := fixable(ctx, q, next.IssueID); err != nil {
				return err
			} else if !fix {
				if _, err := tx.Exec(ctx, "update autofix_runs set state = 'expired', updated_at = now() where id = $1", next.ID); err != nil {
					return err
				}
				continue
			}
			r, err := q.ClaimAutofixRun(ctx, db.ClaimAutofixRunParams{ID: next.ID, TokenID: pgtype.Int8{Int64: tokenID, Valid: true}, LeaseUntil: ts(now.Add(Lease))})
			if err != nil {
				return err
			}
			kind, run, ok = TaskFix, r, true
			return nil
		}
	})
	return kind, run, ok, err
}

// PR is what the daemon reports after opening a pull request.
type PR struct {
	Branch         string
	URL            string
	Summary        string
	Files          []string
	RequiredChecks bool // the default branch requires status checks
	AutoMerge      bool // the daemon turned auto-merge on
}

// OpenedPR records the pull request. A rule-approved run then waits for a
// person to approve the merge; without required checks nothing merges on
// its own and the founder merges on GitHub.
func OpenedPR(ctx context.Context, pool *pgxpool.Pool, projectID, runID int64, pr PR) (db.AutofixRun, error) {
	q := db.New(pool)
	cur, err := q.GetAutofixRun(ctx, db.GetAutofixRunParams{ID: runID, ProjectID: projectID})
	if errors.Is(err, pgx.ErrNoRows) {
		return cur, ErrNotAllowed
	}
	if err != nil {
		return cur, err
	}
	state := PROpened
	autoMerge := pr.AutoMerge && cur.ApprovedBy.String == "user" && pr.RequiredChecks
	if cur.ApprovedBy.String == "rule" && pr.RequiredChecks {
		state = AwaitingMergeApproval
	}
	r, err := q.SetAutofixPR(ctx, db.SetAutofixPRParams{ID: runID, ProjectID: projectID, State: state, Branch: pr.Branch, PrUrl: pr.URL,
		AutoMerge: autoMerge, Summary: clip(pr.Summary, 2000), Files: clipFiles(pr.Files)})
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotAllowed
	}
	if err == nil && !pr.RequiredChecks {
		_ = q.SetAutofixNote(ctx, db.SetAutofixNoteParams{ID: runID, ProjectID: projectID,
			Error: "The default branch doesn't require status checks, so auto-merge stays off. Review and merge the pull request on GitHub."})
	}
	return r, err
}

// MarkMerged records the merge and reports the fix to the verification
// loop, which then watches for the error on the next version.
//
// The fix is reported as of mergedAt, not when the daemon noticed: an app
// that deploys on merge may publish the fixed version first, and
// verification watches for versions first seen after the report. mergedAt
// is clamped to the run's lifetime; zero means now.
func MarkMerged(ctx context.Context, pool *pgxpool.Pool, projectID, runID, tokenID int64, mergedAt, now time.Time) (db.AutofixRun, error) {
	r, err := db.New(pool).SetAutofixMerged(ctx, db.SetAutofixMergedParams{ID: runID, ProjectID: projectID})
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotAllowed
	}
	if err != nil {
		return r, err
	}
	summary := r.Summary
	if summary == "" {
		summary = "Auto-fix pull request merged: " + r.PrUrl
	}
	at := mergedAt
	if at.IsZero() || at.After(now) {
		at = now
	}
	if at.Before(r.CreatedAt.Time) {
		at = r.CreatedAt.Time
	}
	_, err = verify.ReportFix(ctx, pool, projectID, r.IssueID, verify.Report{Source: "agent", TokenID: tokenID, Summary: summary, Files: r.Files}, at)
	if errors.Is(err, verify.ErrTooManyAttempts) || errors.Is(err, verify.ErrNotFixable) {
		err = nil // merged anyway; nothing to verify
	}
	return r, err
}

// MarkFailed ends a run the daemon couldn't finish.
//
// kind is why, when the daemon knows (FailNotInRepo), or "". Daemons older
// than 0.3.0 don't send it but pass the agent's words on, so the tag the
// prompt asks for is also recognized in the message.
func MarkFailed(ctx context.Context, pool *pgxpool.Pool, projectID, runID int64, reason, kind string) error {
	if reason = strings.TrimSpace(reason); reason == "" {
		reason = "The agent couldn't fix it."
	}
	if kind != FailNotInRepo {
		kind = ""
	}
	if i := strings.Index(reason, NotInRepoTag); i >= 0 {
		kind = FailNotInRepo
		reason = strings.TrimSpace(reason[:i] + strings.TrimLeft(reason[i+len(NotInRepoTag):], " :"))
	}
	_, err := db.New(pool).SetAutofixFailed(ctx, db.SetAutofixFailedParams{ID: runID, ProjectID: projectID, Error: clip(reason, 1000), FailReason: kind})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotAllowed
	}
	return err
}

// Note records a problem that doesn't end the run.
func Note(ctx context.Context, pool *pgxpool.Pool, projectID, runID int64, note string) error {
	return db.New(pool).SetAutofixNote(ctx, db.SetAutofixNoteParams{ID: runID, ProjectID: projectID, Error: clip(note, 1000)})
}

// Heartbeat extends a claimed run's lease while the agent works.
func Heartbeat(ctx context.Context, pool *pgxpool.Pool, projectID, runID int64, now time.Time) error {
	n, err := db.New(pool).ExtendAutofixLease(ctx, db.ExtendAutofixLeaseParams{ID: runID, ProjectID: projectID, LeaseUntil: ts(now.Add(Lease))})
	if err == nil && n == 0 {
		return ErrNotAllowed
	}
	return err
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	// Cut on a character boundary: half a character isn't valid UTF-8,
	// and Postgres refuses it.
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func clipFiles(files []string) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		if f = strings.TrimSpace(f); f != "" && len(f) <= 300 && len(out) < 50 {
			out = append(out, f)
		}
	}
	return out
}

// Mailer sends the auto-fix emails (alerts.Notifier).
type Mailer interface {
	AutofixApproval(ctx context.Context, r db.AutofixRun) error
	AutofixOffline(ctx context.Context, r db.AutofixRun) error
}

// SweepStats says what Sweep did.
type SweepStats struct{ LeasesFailed, Expired, Emailed int }

// Sweep runs every minute: runs whose daemon went quiet fail, unanswered
// approvals expire, and approved runs whose project has no daemon online get
// the fix prompt by email instead.
func Sweep(ctx context.Context, pool *pgxpool.Pool, m Mailer, now time.Time) (SweepStats, error) {
	var st SweepStats
	q := db.New(pool)
	failed, err := q.FailExpiredAutofixLeases(ctx, ts(now))
	if err != nil {
		return st, err
	}
	st.LeasesFailed = len(failed)
	n, err := q.ExpireStaleAutofixApprovals(ctx, ts(now.Add(-ApprovalTTL)))
	if err != nil {
		return st, err
	}
	st.Expired = int(n)
	waiting, err := q.ApprovedAutofixRunsWaiting(ctx, ts(now.Add(-OfflineAfter)))
	if err != nil {
		return st, err
	}
	for _, r := range waiting {
		// Mark first: a crash after this never sends twice.
		ok, err := q.MarkAutofixEmailed(ctx, r.ID)
		if err != nil {
			return st, err
		}
		if ok == 0 || m == nil {
			continue
		}
		if err := m.AutofixOffline(ctx, r); err != nil {
			return st, fmt.Errorf("auto-fix offline email for run %d: %w", r.ID, err)
		}
		st.Emailed++
	}
	return st, nil
}

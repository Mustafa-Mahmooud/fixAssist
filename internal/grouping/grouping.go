// Package grouping turns stored events into issues: issue upsert, distinct
// and hourly users, rolling samples, release tracking, regression detection
// and the impact score. It runs inside the group_event job.
package grouping

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/fixassist/internal/ingest"
	"github.com/yourname/fixassist/internal/normalize"
	"github.com/yourname/fixassist/internal/store/db"
	"github.com/yourname/fixassist/internal/verify"
)

// MaxSamples is the rolling number of full events kept per issue (FDR).
const MaxSamples = 20

// Analysis threshold (FDR "AI trigger"): 2+ users or 3+ events.
const (
	AnalyzeMinUsers  = 2
	AnalyzeMinEvents = 3
)

// Result describes what grouping one event changed.
type Result struct {
	Skipped        bool // already grouped (job retry) or event gone
	IssueID        int64
	NewIssue       bool
	NewUser        bool
	NewRelease     bool
	Regressed      bool
	ReachedAnalyze bool // this event made the issue cross the analysis threshold
	Impact         float64
}

// Key-page names must be a whole path segment (or start one, like
// "checkout-success"), so "/carton" is not a cart.
var reKeyPage = regexp.MustCompile(`(?i)(^|/)(checkout|sign-?up|sign-?in|log-?in|register|payment|pay|cart|billing|subscribe|onboarding)([-_/]|$)`)

// RouteWeight is 3 for key pages (checkout, signup, login), else 1. The
// project's key_pages list wins when set; otherwise route names are guessed.
func RouteWeight(route string, keyPages []string) float64 {
	if len(keyPages) > 0 {
		for _, k := range keyPages {
			if k != "" && (route == k || strings.HasPrefix(route, strings.TrimRight(k, "/")+"/")) {
				return 3
			}
		}
		return 1
	}
	if reKeyPage.MatchString(route) {
		return 3
	}
	return 1
}

// Impact is affected users in the last 24 h × route weight (at least 1 user,
// so issues from events without any identity still rank).
func Impact(recentUsers int64, route string, keyPages []string) float64 {
	if recentUsers < 1 {
		recentUsers = 1
	}
	return float64(recentUsers) * RouteWeight(route, keyPages)
}

func cut(s string, n int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if len([]rune(s)) > n {
		return string([]rune(s)[:n-1]) + "…"
	}
	return s
}

// Describe returns an issue title and signature from a stored payload.
// Titles are plain until the AI explanation (Phase 4) replaces them.
func Describe(kind string, payload []byte, tags normalize.Tags) (title, signature string) {
	if payload == nil {
		return kind + " issue", kind
	}
	ev, err := ingest.ParseEvent(payload)
	if err != nil {
		return kind + " issue", kind
	}
	switch kind {
	case ingest.KindNet:
		if h, ok := ev.HTTP(); ok {
			n := ingest.NetFailure(h)
			res, _ := normalize.NetDetail(n)
			path := normalize.RouteTemplate(n.URL)
			msg := res.Message
			if msg == "" {
				msg = n.Error
			}
			if n.Status == 0 {
				title = strings.ToUpper(n.Method) + " " + path + " failed: " + msg
			} else {
				title = strings.ToUpper(n.Method) + " " + path + " returned " + strconv.Itoa(n.Status)
				if msg != "" {
					title += ": " + msg
				}
			}
			return cut(title, 140), normalize.NetSignature(n, tags)
		}
	case ingest.KindJS:
		if ex := ev.MainException(); ex != nil {
			js := ingest.JSError(ev)
			return cut(ex.Type+": "+ex.Value, 140), normalize.JSSignatureV2(js, tags)
		}
	}
	msg := ev.MessageText()
	return cut(msg, 140), "msg \"" + normalize.Message(msg) + "\" route=" + normalize.RouteTemplate(ev.PageURL())
}

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }
func ts(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// GroupEvent groups one stored event in a single transaction. The event row
// is locked and grouped_at checked first, so a retried job never counts twice.
func GroupEvent(ctx context.Context, pool *pgxpool.Pool, eventID int64, receivedAt time.Time, now time.Time) (Result, error) {
	var res Result
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		res = Result{}
		q := db.New(tx)
		ev, err := q.LockEventForGrouping(ctx, db.LockEventForGroupingParams{ID: eventID, ReceivedAt: ts(receivedAt)})
		if errors.Is(err, pgx.ErrNoRows) {
			res.Skipped = true // partition dropped by retention
			return nil
		}
		if err != nil {
			return err
		}
		if ev.GroupedAt.Valid {
			res.Skipped = true
			return nil
		}
		seen := ev.ReceivedAt.Time
		release := ev.ReleaseID.String
		title, sig := Describe(ev.Kind, ev.Payload, normalize.Tags{Platform: ev.TargetAgent, Stack: ev.Backend})

		issue, err := q.UpsertIssue(ctx, db.UpsertIssueParams{
			ProjectID: ev.ProjectID, Fingerprint: ev.Fingerprint, Kind: ev.Kind,
			Signature: sig, Title: title, Route: ev.Route, SeenAt: ts(seen), ReleaseID: text(release),
		})
		if err != nil {
			return err
		}
		res.IssueID, res.NewIssue = issue.ID, issue.Inserted
		userCount := issue.UserCount

		// Users: insert-or-nothing tells us atomically whether the user is new;
		// "last_seen before this hour" tells us whether they are new this hour.
		newThisHour := int32(0)
		hour := seen.UTC().Truncate(time.Hour)
		if ev.UserHash.Valid {
			n, err := q.InsertIssueUser(ctx, db.InsertIssueUserParams{IssueID: issue.ID, UserHash: ev.UserHash.String, FirstSeenAt: ts(seen)})
			if err != nil {
				return err
			}
			if n == 1 {
				res.NewUser, newThisHour = true, 1
				if userCount, err = q.BumpIssueUserCount(ctx, issue.ID); err != nil {
					return err
				}
			} else {
				m, err := q.TouchIssueUserNewHour(ctx, db.TouchIssueUserNewHourParams{SeenAt: ts(seen), IssueID: issue.ID, UserHash: ev.UserHash.String, HourStart: ts(hour)})
				if err != nil {
					return err
				}
				if m == 1 {
					newThisHour = 1
				} else if err := q.TouchIssueUser(ctx, db.TouchIssueUserParams{SeenAt: ts(seen), IssueID: issue.ID, UserHash: ev.UserHash.String}); err != nil {
					return err
				}
			}
		}
		if err := q.BumpIssueHourly(ctx, db.BumpIssueHourlyParams{IssueID: issue.ID, Hour: ts(hour), NewUsers: newThisHour}); err != nil {
			return err
		}

		if ev.Payload != nil {
			if err := q.InsertIssueSample(ctx, db.InsertIssueSampleParams{IssueID: issue.ID, EventID: ev.ID, ReceivedAt: ts(seen), ReleaseID: text(release), Payload: ev.Payload}); err != nil {
				return err
			}
			if issue.EventCount > MaxSamples {
				if err := q.TrimIssueSamples(ctx, db.TrimIssueSamplesParams{IssueID: issue.ID, Keep: MaxSamples}); err != nil {
					return err
				}
			}
		}

		var releaseFirstSeen time.Time
		if release != "" {
			r, err := q.UpsertRelease(ctx, db.UpsertReleaseParams{ProjectID: ev.ProjectID, ReleaseID: release, FirstSeenAt: ts(seen)})
			if err != nil {
				return err
			}
			res.NewRelease = r.Inserted
			releaseFirstSeen = r.FirstSeenAt.Time
		}

		// Regression (FDR verification rule 3): a fixed or verifying issue
		// reappears on the watched release or a newer one. Events from older
		// releases (tabs that haven't reloaded since the fix) don't count.
		if (issue.Status == "fixed" || issue.Status == "verifying") && release != "" &&
			release != issue.FixedReleaseID.String &&
			(!issue.WatchSince.Valid || !releaseFirstSeen.Before(issue.WatchSince.Time)) {
			n, err := q.ReopenIssue(ctx, db.ReopenIssueParams{At: ts(now), ID: issue.ID})
			if err != nil {
				return err
			}
			res.Regressed = n == 1
			if res.Regressed {
				if err := verify.Record(ctx, q, issue.ID, verify.Failed, verify.Implicit, release, 0); err != nil {
					return err
				}
			}
		}

		recent, err := q.CountRecentIssueUsers(ctx, db.CountRecentIssueUsersParams{IssueID: issue.ID, LastSeenAt: ts(now.Add(-24 * time.Hour))})
		if err != nil {
			return err
		}
		res.Impact = Impact(recent, ev.Route.String, ev.KeyPages)
		if err := q.SetIssueImpact(ctx, db.SetIssueImpactParams{ID: issue.ID, ImpactScore: numeric(res.Impact)}); err != nil {
			return err
		}

		before := issue.EventCount-1 >= AnalyzeMinEvents || (userCount-boolInt(res.NewUser)) >= AnalyzeMinUsers
		after := issue.EventCount >= AnalyzeMinEvents || userCount >= AnalyzeMinUsers
		res.ReachedAnalyze = after && !before

		return q.MarkEventIssue(ctx, db.MarkEventIssueParams{IssueID: pgtype.Int8{Int64: issue.ID, Valid: true}, ID: ev.ID, ReceivedAt: ev.ReceivedAt})
	})
	return res, err
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func numeric(f float64) pgtype.Numeric {
	var n pgtype.Numeric
	_ = n.Scan(strconv.FormatFloat(f, 'f', 2, 64))
	return n
}

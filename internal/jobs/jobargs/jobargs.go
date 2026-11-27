// Package jobargs holds River job argument types. It has no dependencies, so
// both the enqueuers (ingest) and the workers (jobs) can import it.
package jobargs

import (
	"time"

	"github.com/riverqueue/river"
)

// Queues (FDR job pipeline).
const (
	QueueGrouping    = "grouping"
	QueueMaintenance = "maintenance"
)

// GroupEvent points at one stored event. Events are partitioned by
// received_at, so both columns are needed to find the row cheaply.
type GroupEvent struct {
	EventID    int64     `json:"event_id"`
	ReceivedAt time.Time `json:"received_at"`
}

func (GroupEvent) Kind() string { return "group_event" }

func (GroupEvent) InsertOpts() river.InsertOpts { return river.InsertOpts{Queue: QueueGrouping} }

// More queues (FDR: a slow LLM provider must never delay grouping).
const (
	QueueAI     = "ai"
	QueueAlerts = "alerts"
)

// AnalyzeIssue explains an issue. Seq makes each trigger unique: 0 for the
// first threshold crossing, the regression time for re-analysis after a
// failed fix. Unique by args, so the same trigger never runs twice.
type AnalyzeIssue struct {
	IssueID int64  `json:"issue_id"`
	Reason  string `json:"reason"` // threshold, regression
	Seq     int64  `json:"seq"`
}

func (AnalyzeIssue) Kind() string { return "analyze_issue" }

func (AnalyzeIssue) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueAI, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

// AlertCheck decides whether an explained issue deserves an instant email.
type AlertCheck struct {
	IssueID int64 `json:"issue_id"`
}

func (AlertCheck) Kind() string { return "alert_check" }

func (AlertCheck) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueAlerts, UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: time.Hour}}
}

// DailyDigest is the hourly sweep that sends 09:00 local-time digests.
type DailyDigest struct{}

func (DailyDigest) Kind() string { return "daily_digest" }

func (DailyDigest) InsertOpts() river.InsertOpts { return river.InsertOpts{Queue: QueueAlerts} }

// VerifyFixes runs the implicit verification rules (every 15 minutes).
type VerifyFixes struct{}

func (VerifyFixes) Kind() string { return "verify_fixes" }

func (VerifyFixes) InsertOpts() river.InsertOpts { return river.InsertOpts{Queue: river.QueueDefault} }

// RollupHourly rebuilds today's and yesterday's analytics rollups.
type RollupHourly struct{}

func (RollupHourly) Kind() string { return "rollup_hourly" }

func (RollupHourly) InsertOpts() river.InsertOpts { return river.InsertOpts{Queue: QueueMaintenance} }

// RollupToday rebuilds only today's rollups (every minute), so the
// dashboard's live pages show new visits within about a minute. It shares
// the single-worker maintenance queue with RollupHourly, so the two never
// rebuild the same day at once.
type RollupToday struct{}

func (RollupToday) Kind() string { return "rollup_today" }

func (RollupToday) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueMaintenance, UniqueOpts: river.UniqueOpts{ByPeriod: time.Minute}}
}

// Partitions runs partition maintenance.
type Partitions struct{}

func (Partitions) Kind() string { return "partitions" }

func (Partitions) InsertOpts() river.InsertOpts { return river.InsertOpts{Queue: QueueMaintenance} }

// AutofixSweep fails auto-fix runs whose daemon went quiet, expires old
// approval requests and emails approved runs no daemon is online for
// (every minute).
type AutofixSweep struct{}

func (AutofixSweep) Kind() string { return "autofix_sweep" }

func (AutofixSweep) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueAlerts, UniqueOpts: river.UniqueOpts{ByPeriod: time.Minute}}
}

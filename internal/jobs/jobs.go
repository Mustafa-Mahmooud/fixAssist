// Package jobs defines River job args, workers and the River client setup.
package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/yourname/fixassist/internal/ai"
	"github.com/yourname/fixassist/internal/alerts"
	"github.com/yourname/fixassist/internal/analytics"
	"github.com/yourname/fixassist/internal/grouping"
	"github.com/yourname/fixassist/internal/jobs/jobargs"
	"github.com/yourname/fixassist/internal/store"
	"github.com/yourname/fixassist/internal/store/db"
	"github.com/yourname/fixassist/internal/verify"
)

// GroupEventWorker groups one event into an issue (internal/grouping).
// Idempotent: the event row is locked and grouped_at checked in the same
// transaction.
type GroupEventWorker struct {
	river.WorkerDefaults[jobargs.GroupEvent]
	Pool *pgxpool.Pool
	Log  *slog.Logger
}

func (w *GroupEventWorker) Work(ctx context.Context, job *river.Job[jobargs.GroupEvent]) error {
	res, err := grouping.GroupEvent(ctx, w.Pool, job.Args.EventID, job.Args.ReceivedAt, time.Now())
	if err != nil {
		return err
	}
	if res.Skipped {
		return nil
	}
	if res.NewIssue || res.Regressed {
		w.Log.Info("issue", "issue_id", res.IssueID, "new", res.NewIssue, "regressed", res.Regressed, "impact", res.Impact)
	}
	// FDR AI trigger: first time an issue reaches 2 people or 3 events; and
	// again after a regression, telling the agent the first fix didn't work.
	var analyze *jobargs.AnalyzeIssue
	switch {
	case res.Regressed:
		analyze = &jobargs.AnalyzeIssue{IssueID: res.IssueID, Reason: "regression", Seq: time.Now().Unix()}
	case res.ReachedAnalyze:
		analyze = &jobargs.AnalyzeIssue{IssueID: res.IssueID, Reason: "threshold"}
	}
	if analyze != nil {
		client, err := river.ClientFromContextSafely[pgx.Tx](ctx)
		if err != nil {
			return err
		}
		if _, err := client.Insert(ctx, *analyze, nil); err != nil {
			return err
		}
	}
	return nil
}

// AnalyzeWorker explains an issue (agent mode: catalog + agent prompt) and
// queues the instant-alert check.
type AnalyzeWorker struct {
	river.WorkerDefaults[jobargs.AnalyzeIssue]
	Pool *pgxpool.Pool
	Log  *slog.Logger
	Code ai.CodeSearcher // optional: GitHub code context
}

func (w *AnalyzeWorker) Work(ctx context.Context, job *river.Job[jobargs.AnalyzeIssue]) error {
	attempt := 1
	if job.Args.Reason == "regression" {
		attempt = ai.AttemptAfterFailure
	}
	id, s, err := ai.AnalyzeWith(ctx, w.Pool, job.Args.IssueID, attempt, w.Code)
	if err != nil {
		return err
	}
	w.Log.Info("issue explained", "issue_id", job.Args.IssueID, "suggestion_id", id, "origin", s.Origin, "catalog", s.CatalogKey, "severity", s.Severity)
	client, err := river.ClientFromContextSafely[pgx.Tx](ctx)
	if err != nil {
		return err
	}
	_, err = client.Insert(ctx, jobargs.AlertCheck{IssueID: job.Args.IssueID}, nil)
	return err
}

// AlertCheckWorker applies the instant-alert rules.
type AlertCheckWorker struct {
	river.WorkerDefaults[jobargs.AlertCheck]
	Notifier *alerts.Notifier
	Log      *slog.Logger
}

func (w *AlertCheckWorker) Work(ctx context.Context, job *river.Job[jobargs.AlertCheck]) error {
	if w.Notifier == nil {
		return nil
	}
	res, err := w.Notifier.CheckInstant(ctx, job.Args.IssueID)
	if err == nil {
		w.Log.Info("alert check", "issue_id", job.Args.IssueID, "result", res)
	}
	return err
}

// DigestWorker is the hourly digest sweep.
type DigestWorker struct {
	river.WorkerDefaults[jobargs.DailyDigest]
	Notifier *alerts.Notifier
	Log      *slog.Logger
}

func (w *DigestWorker) Work(ctx context.Context, _ *river.Job[jobargs.DailyDigest]) error {
	if w.Notifier == nil {
		return nil
	}
	n, err := w.Notifier.RunDigests(ctx)
	if n > 0 {
		w.Log.Info("digests sent", "count", n)
	}
	if err != nil {
		return err
	}
	q, err := w.Notifier.CheckQuotas(ctx)
	if q > 0 {
		w.Log.Info("quota warnings sent", "count", q)
	}
	if err != nil {
		return err
	}
	g, err := w.Notifier.RunGrowthReports(ctx)
	if g > 0 {
		w.Log.Info("growth reports", "count", g)
	}
	return err
}

// RollupWorker rebuilds analytics rollups (Phase 6).
type RollupWorker struct {
	river.WorkerDefaults[jobargs.RollupHourly]
	Pool *pgxpool.Pool
}

func (w *RollupWorker) Work(ctx context.Context, _ *river.Job[jobargs.RollupHourly]) error {
	return analytics.Run(ctx, w.Pool, time.Now())
}

// RollupTodayWorker rebuilds just today's rollups for the live dashboard.
type RollupTodayWorker struct {
	river.WorkerDefaults[jobargs.RollupToday]
	Pool *pgxpool.Pool
}

func (w *RollupTodayWorker) Work(ctx context.Context, _ *river.Job[jobargs.RollupToday]) error {
	return analytics.RollupDay(ctx, w.Pool, time.Now().UTC())
}

// VerifyWorker runs the implicit verification rules.
type VerifyWorker struct {
	river.WorkerDefaults[jobargs.VerifyFixes]
	Pool *pgxpool.Pool
	Log  *slog.Logger
}

func (w *VerifyWorker) Work(ctx context.Context, _ *river.Job[jobargs.VerifyFixes]) error {
	st, err := verify.Run(ctx, w.Pool, verify.DefaultRules, time.Now())
	if st != (verify.Stats{}) {
		w.Log.Info("verification", "started", st.StartedVerifying, "fixed", st.VerifiedFixed, "probably_fixed", st.ProbablyFixed)
	}
	return err
}

// PartitionsWorker creates the next 7 days of partitions and drops those
// past retention. Safe to rerun.
type PartitionsWorker struct {
	river.WorkerDefaults[jobargs.Partitions]
	Pool *pgxpool.Pool
	Log  *slog.Logger
}

func (w *PartitionsWorker) Work(ctx context.Context, _ *river.Job[jobargs.Partitions]) error {
	return RunPartitions(ctx, w.Pool, w.Log, time.Now())
}

// RunPartitions is the partitions job body, also used at startup and by
// `app retention run`.
func RunPartitions(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, now time.Time) error {
	if _, err := store.EnsurePartitions(ctx, pool, now, 1, 7); err != nil {
		return fmt.Errorf("ensure partitions: %w", err)
	}
	dropped, err := store.DropOldPartitions(ctx, pool, now, store.RetentionDays)
	if err != nil {
		return fmt.Errorf("drop partitions: %w", err)
	}
	if len(dropped) > 0 {
		log.Info("dropped old partitions", "partitions", dropped)
	}
	// Expired magic-link tokens and sessions.
	if err := db.New(pool).PruneAuth(ctx); err != nil {
		return fmt.Errorf("prune auth: %w", err)
	}
	return nil
}

// NewClient builds the River client. With runWorkers false it is insert-only
// (an ingest-only box); with true it also processes the queues.
func NewClient(pool *pgxpool.Pool, log *slog.Logger, runWorkers bool) (*river.Client[pgx.Tx], error) {
	return NewClientWith(Deps{Pool: pool, Log: log, RunWorkers: runWorkers})
}

// Deps are what the workers need. Notifier may be nil (alerts are skipped).
type Deps struct {
	Pool       *pgxpool.Pool
	Log        *slog.Logger
	Notifier   *alerts.Notifier
	Code       ai.CodeSearcher // optional: GitHub code context
	RunWorkers bool
}

// NewClientWith builds the River client with all workers.
func NewClientWith(d Deps) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, &GroupEventWorker{Pool: d.Pool, Log: d.Log})
	river.AddWorker(workers, &PartitionsWorker{Pool: d.Pool, Log: d.Log})
	river.AddWorker(workers, &AnalyzeWorker{Pool: d.Pool, Log: d.Log, Code: d.Code})
	river.AddWorker(workers, &AlertCheckWorker{Notifier: d.Notifier, Log: d.Log})
	river.AddWorker(workers, &DigestWorker{Notifier: d.Notifier, Log: d.Log})
	river.AddWorker(workers, &VerifyWorker{Pool: d.Pool, Log: d.Log})
	river.AddWorker(workers, &RollupWorker{Pool: d.Pool})
	river.AddWorker(workers, &RollupTodayWorker{Pool: d.Pool})

	cfg := &river.Config{Workers: workers, Logger: d.Log.With("component", "river")}
	if d.RunWorkers {
		cfg.Queues = map[string]river.QueueConfig{
			jobargs.QueueGrouping:    {MaxWorkers: 16},
			jobargs.QueueAI:          {MaxWorkers: 4},
			jobargs.QueueAlerts:      {MaxWorkers: 2},
			jobargs.QueueMaintenance: {MaxWorkers: 1},
			river.QueueDefault:       {MaxWorkers: 2},
		}
		cfg.PeriodicJobs = []*river.PeriodicJob{
			river.NewPeriodicJob(river.PeriodicInterval(24*time.Hour),
				func() (river.JobArgs, *river.InsertOpts) { return jobargs.Partitions{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true}),
			river.NewPeriodicJob(river.PeriodicInterval(time.Hour),
				func() (river.JobArgs, *river.InsertOpts) { return jobargs.DailyDigest{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true}),
			river.NewPeriodicJob(river.PeriodicInterval(time.Hour),
				func() (river.JobArgs, *river.InsertOpts) { return jobargs.RollupHourly{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true}),
			river.NewPeriodicJob(river.PeriodicInterval(time.Minute),
				func() (river.JobArgs, *river.InsertOpts) { return jobargs.RollupToday{}, nil },
				nil),
			river.NewPeriodicJob(river.PeriodicInterval(15*time.Minute),
				func() (river.JobArgs, *river.InsertOpts) { return jobargs.VerifyFixes{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true}),
		}
	}
	return river.NewClient(riverpgxv5.New(d.Pool), cfg)
}

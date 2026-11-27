package web

import (
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yourname/fixassist/internal/store/db"
)

// Time to set up: for apps added in the last 30 days, how long each setup
// step took from adding the app, and whether the setup reminders helped.
// The activation funnel says how many got there; this says how fast, which
// is what changes when setup gets easier.

type setupStep struct {
	Label         string
	Reached, Of   int
	Median        string // from adding the app; "—" when nobody got there
	WithinHour    string // share of all apps that got there within an hour
	WithinDay     string
	WithinHourPct int // for the bar
}

type setupNudges struct {
	InstallSent, InstallThenConnected int
	TestSent, TestThenIssue           int
}

func setupTimes(rows []db.AdminSetupTimesRow) ([]setupStep, setupNudges) {
	steps := []struct {
		label string
		at    func(r db.AdminSetupTimesRow) pgtype.Timestamptz
	}{
		{"First visit", func(r db.AdminSetupTimesRow) pgtype.Timestamptz { return r.ConnectedAt }},
		{"First issue", func(r db.AdminSetupTimesRow) pgtype.Timestamptz { return r.IssueAt }},
		{"First explanation", func(r db.AdminSetupTimesRow) pgtype.Timestamptz { return r.ExplainedAt }},
		{"First confirmed fix", func(r db.AdminSetupTimesRow) pgtype.Timestamptz { return r.FixedAt }},
	}
	var out []setupStep
	for _, s := range steps {
		var ds []time.Duration
		hour, day := 0, 0
		for _, r := range rows {
			at := s.at(r)
			if !at.Valid {
				continue
			}
			d := at.Time.Sub(r.CreatedAt.Time)
			if d < 0 {
				d = 0
			}
			ds = append(ds, d)
			if d <= time.Hour {
				hour++
			}
			if d <= 24*time.Hour {
				day++
			}
		}
		st := setupStep{Label: s.label, Reached: len(ds), Of: len(rows), Median: "—",
			WithinHour: percent(int64(hour), int64(len(rows))), WithinDay: percent(int64(day), int64(len(rows)))}
		if len(rows) > 0 {
			st.WithinHourPct = hour * 100 / len(rows)
		}
		if len(ds) > 0 {
			sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
			st.Median = shortDuration(ds[len(ds)/2])
		}
		out = append(out, st)
	}
	var n setupNudges
	for _, r := range rows {
		if r.InstallNudgeAt.Valid {
			n.InstallSent++
			if r.ConnectedAt.Valid && r.ConnectedAt.Time.After(r.InstallNudgeAt.Time) {
				n.InstallThenConnected++
			}
		}
		if r.TestNudgeAt.Valid {
			n.TestSent++
			if r.IssueAt.Valid && r.IssueAt.Time.After(r.TestNudgeAt.Time) {
				n.TestThenIssue++
			}
		}
	}
	return out, n
}

// shortDuration: "40 s", "12 min", "5 h", "3 days".
func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

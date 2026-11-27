package web

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yourname/fixassist/internal/store/db"
)

func TestSetupTimes(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	at := func(d time.Duration) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t0.Add(d), Valid: true} }
	none := pgtype.Timestamptz{}
	rows := []db.AdminSetupTimesRow{
		{CreatedAt: at(0), ConnectedAt: at(5 * time.Minute), IssueAt: at(10 * time.Minute), ExplainedAt: at(11 * time.Minute)},
		{CreatedAt: at(0), ConnectedAt: at(30 * time.Hour), InstallNudgeAt: at(25 * time.Hour)},
		{CreatedAt: at(0), ConnectedAt: at(20 * time.Minute), TestNudgeAt: at(73 * time.Hour), IssueAt: at(74 * time.Hour)},
		{CreatedAt: at(0), ConnectedAt: none, InstallNudgeAt: at(25 * time.Hour)},
	}
	steps, n := setupTimes(rows)
	visit := steps[0]
	if visit.Reached != 3 || visit.Of != 4 || visit.Median != "20 min" || visit.WithinHour != "50%" || visit.WithinDay != "50%" {
		t.Errorf("first visit: %+v", visit)
	}
	if steps[1].Reached != 2 || steps[3].Median != "—" {
		t.Errorf("steps: %+v", steps)
	}
	if n != (setupNudges{InstallSent: 2, InstallThenConnected: 1, TestSent: 1, TestThenIssue: 1}) {
		t.Errorf("nudges: %+v", n)
	}
	if shortDuration(40*time.Second) != "40 s" || shortDuration(5*time.Hour) != "5 h" || shortDuration(72*time.Hour) != "3 days" {
		t.Error("shortDuration")
	}
}

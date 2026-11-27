package alerts_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yourname/fixassist/internal/alerts"
	"github.com/yourname/fixassist/internal/jobs"
	"github.com/yourname/fixassist/internal/projects"
	"github.com/yourname/fixassist/internal/sitecheck"
	"github.com/yourname/fixassist/internal/store"
)

// Setup reminders: one "finish setup" email with what the site check found,
// one "send a test error" email, each once, only in the founder's daytime.
func TestSetupNudges(t *testing.T) {
	url := os.Getenv("FIX_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("FIX_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, "drop schema public cascade; create schema public"); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, pool, log); err != nil {
		t.Fatal(err)
	}
	if err := jobs.RunPartitions(ctx, pool, log, time.Now()); err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 11, 27, 3, 0, 0, 0, time.UTC) // 3 a.m. UTC
	svc := &projects.Service{Pool: pool}
	mk := func(name string, age time.Duration) int64 {
		p, err := svc.Create(ctx, projects.CreateParams{Org: "acme", Name: name, Agent: "lovable", AllowedDomains: []string{name + ".lovable.app"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "update projects set created_at = $2, timezone = 'UTC' where id = $1", p.ID, clock.Add(-age)); err != nil {
			t.Fatal(err)
		}
		return p.ID
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	stuck := mk("stuck", 30*time.Hour)   // no visit after a day: install reminder
	fresh := mk("fresh", 2*time.Hour)    // too new
	quiet := mk("quiet", 80*time.Hour)   // connected, nothing caught: test reminder
	busy := mk("busy", 80*time.Hour)     // connected with an issue: nothing
	hidden := mk("hidden", 30*time.Hour) // setup list hidden: nothing
	_ = fresh
	exec("insert into project_rejected_origins (project_id, host) values ($1, 'preview--stuck.lovable.app')", stuck)
	exec("insert into pageviews (project_id, route, session_id) values ($1, '/', 's'), ($2, '/', 's')", quiet, busy)
	exec(`insert into issues (project_id, fingerprint, kind, signature, title, route, first_seen_at, last_seen_at, event_count)
		values ($1, 'f', 'js', 's', 'x', '/', now(), now(), 1)`, busy)
	exec("update projects set onboarding_dismissed_at = now() where id = $1", hidden)
	exec(`with u as (insert into users (email) values ('founder@example.com') returning id)
		insert into memberships (user_id, org_id) select u.id, o.id from u, orgs o where o.name = 'acme'`)

	mail := &memSender{}
	var checked []string
	n := &alerts.Notifier{Pool: pool, Sender: mail, BaseURL: "https://fix.example", IngestBase: "https://in.example", Log: log,
		Now: func() time.Time { return clock },
		SiteCheck: func(_ context.Context, s sitecheck.Site) sitecheck.Result {
			checked = append(checked, s.Domains[0])
			return sitecheck.Result{Level: "bad", Title: "The snippet isn't on your live site yet", Text: "Publish your app in " + s.Builder + "."}
		}}

	if sent, err := n.RunSetupNudges(ctx); err != nil || sent != 0 {
		t.Fatalf("at night: sent %d, %v", sent, err)
	}
	clock = clock.Add(8 * time.Hour) // 11 a.m.
	if sent, err := n.RunSetupNudges(ctx); err != nil || sent != 2 {
		t.Fatalf("sent %d, %v; want 2", sent, err)
	}
	if len(checked) != 1 || checked[0] != "stuck.lovable.app" {
		t.Errorf("site checks = %v, want only the stuck app", checked)
	}
	byKind := map[string]string{}
	rows, _ := pool.Query(ctx, "select kind, project_id::text, body from alert_log")
	for rows.Next() {
		var kind, pid, body string
		_ = rows.Scan(&kind, &pid, &body)
		byKind[kind] = pid + "|" + body
	}
	rows.Close()
	if b := byKind["setup_install"]; !strings.HasPrefix(b, itoa(stuck)+"|") || !strings.Contains(b, "preview--stuck.lovable.app") || !strings.Contains(b, "isn't on your live site") || !strings.Contains(b, "Publish your app in Lovable") {
		t.Errorf("install reminder: %s", b)
	}
	if b := byKind["setup_test"]; !strings.HasPrefix(b, itoa(quiet)+"|") || !strings.Contains(b, "https://quiet.lovable.app/?fixassist-test") {
		t.Errorf("test reminder: %s", b)
	}
	if mail.count() != 2 {
		t.Errorf("emails = %d, want 2", mail.count())
	}
	// Never twice.
	clock = clock.Add(time.Hour)
	if sent, err := n.RunSetupNudges(ctx); err != nil || sent != 0 {
		t.Errorf("second run sent %d, %v", sent, err)
	}
	if len(checked) != 1 {
		t.Error("checked the site again after the reminder went out")
	}
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

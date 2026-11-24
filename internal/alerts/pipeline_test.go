package alerts_test

// End to end (FIX_TEST_DATABASE_URL, schema is wiped): envelopes → grouping →
// analyze_issue (threshold) → fix_suggestions → alert_check → alert_log +
// email, then the digest and a regression re-analysis.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/fixassist/internal/ai"
	"github.com/yourname/fixassist/internal/alerts"
	"github.com/yourname/fixassist/internal/ingest"
	"github.com/yourname/fixassist/internal/jobs"
	"github.com/yourname/fixassist/internal/projects"
	"github.com/yourname/fixassist/internal/store"
	"github.com/yourname/fixassist/internal/store/db"
)

type memSender struct {
	mu   sync.Mutex
	sent []alerts.Message
}

func (m *memSender) Send(_ context.Context, msg alerts.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, msg)
	return nil
}

func (m *memSender) count() int { m.mu.Lock(); defer m.mu.Unlock(); return len(m.sent) }

func scalar(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func eventually(cond func() bool) bool {
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
	return true
}

func TestExplainAndAlertPipeline(t *testing.T) {
	url := os.Getenv("FIX_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("FIX_TEST_DATABASE_URL not set")
	}
	files, _ := filepath.Glob("../ingest/testdata/envelopes/*.envelope")
	sort.Strings(files)
	if len(files) == 0 {
		t.Skip("no golden envelopes")
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
	p, err := (&projects.Service{Pool: pool}).Create(ctx, projects.CreateParams{Org: "acme", Name: "Spike", Agent: "lovable", Backend: "supabase"})
	if err != nil {
		t.Fatal(err)
	}
	// "/" is a key page here, so every spike issue has impact 2 people × 3 = 6.
	_, _ = pool.Exec(ctx, "update projects set key_pages = '{/}' where id = $1", p.ID)
	_, _ = pool.Exec(ctx, `with u as (insert into users (email) values ('founder@example.com') returning id)
		insert into memberships (user_id, org_id) select u.id, o.id from u, orgs o where o.name = 'acme'`)

	mail := &memSender{}
	notifier := &alerts.Notifier{Pool: pool, Sender: mail, BaseURL: "https://fix.example", Prompt: ai.PromptForRow, Log: log}
	rc, err := jobs.NewClientWith(jobs.Deps{Pool: pool, Log: log, Notifier: notifier, RunWorkers: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = rc.Stop(c)
	}()

	cache := &projects.Cache{Q: db.New(pool), Log: log}
	if err := cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&ingest.Handler{Sink: &ingest.PGSink{Pool: pool, River: rc}, Log: log, Projects: cache}).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	send := func(path string) {
		b, _ := os.ReadFile(path)
		resp, err := http.Post(fmt.Sprintf("%s/api/%d/envelope/?sentry_key=%s", srv.URL, p.ID, p.PublicKey), "text/plain", strings.NewReader(string(b)))
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("send %s: %v %v", path, err, resp)
		}
		resp.Body.Close()
	}
	for _, f := range files {
		send(f)
	}

	// Each spike issue reaches 2 people with its build-2 event: analyzed once.
	waitFor(t, "8 suggestions", func() bool { return scalar(t, pool, "select count(*) from fix_suggestions") == 8 })
	if n := scalar(t, pool, "select count(*) from issues where status = 'explained' and explained_at is not null"); n != 8 {
		t.Errorf("explained issues = %d, want 8", n)
	}
	if n := scalar(t, pool, "select count(*) from fix_suggestions where origin = 'catalog'"); n != 6 {
		t.Errorf("catalog suggestions = %d, want 6 (2 spike cases are app-specific throws)", n)
	}
	if n := scalar(t, pool, "select count(*) from knowledge_entries"); n != 8 {
		t.Errorf("knowledge entries = %d, want 8", n)
	}

	// Instant alerts: impact 6 ≥ 3 for all 8, but at most 3 per project per day.
	if !eventually(func() bool { return scalar(t, pool, "select count(*) from alert_log where kind = 'instant'") == 3 }) {
		rows, _ := pool.Query(ctx, "select kind, state::text, attempt, coalesce(errors::text, '') from river_job where kind in ('analyze_issue', 'alert_check') order by id")
		for rows.Next() {
			var k, st, e string
			var a int
			_ = rows.Scan(&k, &st, &a, &e)
			t.Logf("job %s %s attempt=%d %s", k, st, a, e)
		}
		rows.Close()
		rows, _ = pool.Query(ctx, "select i.title, i.status, i.impact_score::text from issues i order by id")
		for rows.Next() {
			var a, b, c string
			_ = rows.Scan(&a, &b, &c)
			t.Logf("issue %s %s impact=%s", a, b, c)
		}
		t.Fatalf("instant alerts = %d, want 3", scalar(t, pool, "select count(*) from alert_log where kind = 'instant'"))
	}
	time.Sleep(500 * time.Millisecond) // let the remaining alert checks run
	if n := scalar(t, pool, "select count(*) from alert_log where kind = 'instant'"); n != 3 {
		t.Errorf("instant alerts = %d, want the daily cap of 3", n)
	}
	waitFor(t, "3 emails", func() bool { return mail.count() == 3 })
	m := mail.sent[0]
	if m.To != "founder@example.com" || !strings.HasPrefix(m.Subject, "[Spike] ") ||
		!strings.Contains(m.Text, "Paste this into Lovable:") || !strings.Contains(m.Text, "To check it worked:") ||
		!strings.Contains(m.Text, "https://fix.example/projects/") {
		t.Errorf("alert email:\n%s\n%s", m.Subject, m.Text)
	}

	// Digest: at 09:00 UTC, one summary with the 8 new issues; running again sends nothing.
	notifier.Now = func() time.Time { return time.Now().UTC().Truncate(24 * time.Hour).Add(9*time.Hour + 5*time.Minute) }
	sent, err := notifier.RunDigests(ctx)
	if err != nil || sent != 1 {
		t.Fatalf("digest: sent %d, err %v", sent, err)
	}
	if again, _ := notifier.RunDigests(ctx); again != 0 {
		t.Errorf("second digest run sent %d", again)
	}
	var body string
	_ = pool.QueryRow(ctx, "select body from alert_log where kind = 'digest'").Scan(&body)
	if !strings.Contains(body, "New issues (8)") || !strings.Contains(body, "Still open: 8") {
		t.Errorf("digest body:\n%s", body)
	}

	// Regression: fix the TypeError issue in build 1, send build 2's event
	// again: it reopens and is re-analyzed as a second attempt.
	var issueID int64
	var b1 string
	_ = pool.QueryRow(ctx, "select id, first_release_id from issues where title like 'TypeError%'").Scan(&issueID, &b1)
	_, _ = db.New(pool).MarkIssueFixed(ctx, db.MarkIssueFixedParams{ID: issueID, ProjectID: p.ID, ReleaseID: pgText(b1)})
	var b2File string
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		if strings.Contains(string(raw), `"spike_case":"js_typeerror"`) && !strings.Contains(string(raw), b1) {
			b2File = f
		}
	}
	send(b2File)
	waitFor(t, "second-attempt suggestion", func() bool {
		return scalar(t, pool, "select count(*) from fix_suggestions where issue_id = $1 and attempt = 2", issueID) == 1
	})
	var prompt string
	_ = pool.QueryRow(ctx, "select fix_prompt from fix_suggestions where issue_id = $1 and attempt = 2", issueID).Scan(&prompt)
	if !strings.HasPrefix(prompt, "A previous fix for this problem was applied") {
		t.Errorf("attempt-2 prompt: %q", prompt[:80])
	}
}

func pgText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

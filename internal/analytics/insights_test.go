package analytics_test

import (
	"context"
	"testing"
	"time"

	"github.com/yourname/fixassist/internal/analytics"
)

func TestOutcomeDirections(t *testing.T) {
	up := analytics.Metric{Better: "higher"}
	down := analytics.Metric{Better: "lower"}
	cases := []struct {
		m           analytics.Metric
		base, after float64
		want        string
	}{
		{up, 0.10, 0.12, "worked"},
		{up, 0.10, 0.105, "no_change"},
		{up, 0.10, 0.08, "worse"},
		{down, 4000, 2000, "worked"},
		{down, 0.7, 0.75, "no_change"},
		{down, 0.5, 0.7, "worse"},
		{up, 0, 0.05, "worked"},
		{up, 0, 0, "no_change"},
	}
	for _, c := range cases {
		if got := analytics.Outcome(c.m, c.base, c.after); got != c.want {
			t.Errorf("Outcome(%s, %v → %v) = %s, want %s", c.m.Better, c.base, c.after, got, c.want)
		}
	}
}

func TestDetectGenerateEvaluate(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	now := time.Now().UTC()
	day := func(back int) time.Time { return now.Truncate(24*time.Hour).AddDate(0, 0, -back) }
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	var funnel int64
	if err := f.pool.QueryRow(ctx, `insert into funnels (project_id, name, steps) values ($1, 'Visit to sign-up',
		'[{"kind":"page","match":"/"},{"kind":"page","match":"/pricing"},{"kind":"signup","match":""}]') returning id`, f.p.ID).Scan(&funnel); err != nil {
		t.Fatal(err)
	}
	// Last 7 full days: every detector has something to find.
	for b := 1; b <= 7; b++ {
		d := day(b)
		exec(`insert into daily_funnel_stats (project_id, day, funnel_id, step, device, sessions) values
			($1,$2,$3,0,'desktop',100), ($1,$2,$3,1,'desktop',30), ($1,$2,$3,2,'desktop',10)`, f.p.ID, d, funnel)
		exec(`insert into daily_page_stats (project_id, day, route, device, views, visitors, sessions, exits, load_ms_sum, load_ms_count) values
			($1,$2,'/pricing','mobile',40,40,40,28,0,0), ($1,$2,'/pricing','desktop',40,40,40,12,0,0),
			($1,$2,'/','desktop',50,50,50,5,200000,50)`, f.p.ID, d)
		exec(`insert into daily_overview (project_id, day, visitors, sessions, pageviews, signups) values ($1,$2,200,220,400,20)`, f.p.ID, d)
		exec(`insert into daily_source_stats (project_id, day, source, sessions, visitors, signups) values ($1,$2,'tiktok',50,50,1)`, f.p.ID, d)
	}
	exec(`insert into issues (project_id, fingerprint, kind, signature, title, route, first_seen_at, last_seen_at, event_count, user_count)
		values ($1, 'fp', 'net', 'net POST /auth/v1/signup 422 auth:weak_password "x"', 'Sign-up fails: weak password', '/signup', now(), now(), 5, 5)`, f.p.ID)

	ins, err := analytics.Detect(ctx, f.pool, f.p.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	// Sign-up errors are pinned first; then by sessions affected over the
	// 7 days: slow page (350 × 2 s = 700) beats the funnel drop (700 × 0.7 = 490)
	// and the weak source (280).
	var got []string
	for _, in := range ins {
		got = append(got, in.Detector)
		t.Logf("%s: %s | %s", in.Detector, in.Title, in.Evidence)
	}
	if len(ins) != 3 || got[0] != "signup_errors" || got[1] != "slow_page" || got[2] != "funnel_drop" {
		t.Fatalf("detectors = %v", got)
	}
	if ins[2].Evidence != "Only 30% of 700 sessions went from “visit /” to “visit /pricing” in the last 7 days (70% left on the home page (/))." {
		t.Errorf("funnel evidence = %q", ins[2].Evidence)
	}

	n, err := analytics.Generate(ctx, f.pool, f.p.ID, now)
	if err != nil || n != 3 {
		t.Fatalf("Generate = %d, %v", n, err)
	}
	if again, _ := analytics.Generate(ctx, f.pool, f.p.ID, now); again != 0 {
		t.Errorf("second Generate this week added %d", again)
	}

	// Before/after: nothing to compare until a new release appears.
	exec(`update growth_insights set created_at = now() - interval '40 days'`)
	if done, _ := analytics.Evaluate(ctx, f.pool, now); done != 0 {
		t.Fatalf("evaluated %d without a new release", done)
	}
	// A release 30 days ago; on it the home page got twice as fast.
	exec(`insert into releases (project_id, release_id, first_seen_at, last_seen_at) values ($1, 'r2', now() - interval '30 days', now())`, f.p.ID)
	for b := 23; b <= 29; b++ {
		exec(`insert into daily_page_stats (project_id, day, route, device, views, visitors, sessions, exits, load_ms_sum, load_ms_count)
			values ($1,$2,'/','desktop',50,50,50,5,100000,50)`, f.p.ID, day(b))
	}
	done, err := analytics.Evaluate(ctx, f.pool, now)
	if err != nil || done != 3 {
		t.Fatalf("Evaluate = %d, %v", done, err)
	}
	if s := f.val("select outcome || ':' || after_value::text from growth_insights where detector = 'slow_page'"); s != "worked:2000" {
		t.Errorf("slow page = %s, want worked:2000", s)
	}
	if s := f.val("select outcome from growth_insights where detector = 'funnel_drop'"); s != "worse" {
		t.Errorf("funnel = %s (no funnel data after the release → worse)", s)
	}
}

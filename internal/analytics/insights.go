package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Weekly growth insights (FDR Phase 6), rule-based in agent mode: detectors
// over the rollups, each giving evidence in numbers, a hypothesis (never a
// promise), a prompt for the founder's AI agent and a before/after check.

// MaxInsights per project per week.
const MaxInsights = 3

// Thresholds (starting values; tune on real data like the FDR's N/48 h).
const (
	minSessions      = 20
	funnelDropShare  = 0.5  // a step loses at least half the previous step
	mobileExitGap    = 0.20 // phone exit rate this much above computers
	slowLoadMS       = 3000
	weakSourceFactor = 0.5 // a source converts at under half the overall rate
	changeThreshold  = 0.10
)

// Metric says what to re-measure after the next release. Better tells
// which direction is an improvement.
type Metric struct {
	Type   string `json:"type"` // funnel_step, page_exit, page_load, signup_rate, source_rate
	Funnel int64  `json:"funnel,omitempty"`
	Step   int    `json:"step,omitempty"`
	Route  string `json:"route,omitempty"`
	Device string `json:"device,omitempty"`
	Source string `json:"source,omitempty"`
	Better string `json:"better"` // higher, lower
}

// Insight is one recommendation.
type Insight struct {
	Detector   string
	Title      string
	Evidence   string
	Hypothesis string
	FixPrompt  string
	HowToCheck string
	Metric     Metric
	Baseline   float64
	weight     float64 // sessions affected, for ranking
}

func pct(f float64) string {
	if f > 0 && f < 0.01 {
		return fmt.Sprintf("%.1f%%", f*100)
	}
	return fmt.Sprintf("%.0f%%", f*100)
}

func ratio(a, b int64) float64 {
	if b <= 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// window is [from, to] inclusive days.
type window struct{ from, to time.Time }

func lastDays(now time.Time, days int) window {
	to := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -1) // full days only
	return window{from: to.AddDate(0, 0, -(days - 1)), to: to}
}

// Measure computes a metric over a window from the rollups. sample is the
// number of sessions (or views) behind it.
func Measure(ctx context.Context, q pgxQuerier, projectID int64, m Metric, w window) (value float64, sample int64, err error) {
	switch m.Type {
	case "funnel_step":
		var prev, cur int64
		err = q.QueryRow(ctx, `select coalesce(sum(sessions) filter (where step = $3 - 1), 0), coalesce(sum(sessions) filter (where step = $3), 0)
			from daily_funnel_stats where project_id = $1 and funnel_id = $2 and day between $4 and $5`,
			projectID, m.Funnel, m.Step, w.from, w.to).Scan(&prev, &cur)
		return ratio(cur, prev), prev, err
	case "page_exit":
		var sessions, exits int64
		err = q.QueryRow(ctx, `select coalesce(sum(sessions), 0), coalesce(sum(exits), 0) from daily_page_stats
			where project_id = $1 and route = $2 and ($3 = '' or device = $3) and day between $4 and $5`,
			projectID, m.Route, m.Device, w.from, w.to).Scan(&sessions, &exits)
		return ratio(exits, sessions), sessions, err
	case "page_load":
		var sum, n int64
		err = q.QueryRow(ctx, `select coalesce(sum(load_ms_sum), 0), coalesce(sum(load_ms_count), 0) from daily_page_stats
			where project_id = $1 and route = $2 and day between $3 and $4`, projectID, m.Route, w.from, w.to).Scan(&sum, &n)
		return ratio(sum, n), n, err
	case "signup_rate":
		var visitors, signups int64
		err = q.QueryRow(ctx, `select coalesce(sum(visitors), 0), coalesce(sum(signups), 0) from daily_overview
			where project_id = $1 and day between $2 and $3`, projectID, w.from, w.to).Scan(&visitors, &signups)
		return ratio(signups, visitors), visitors, err
	case "source_rate":
		var sessions, signups int64
		err = q.QueryRow(ctx, `select coalesce(sum(sessions), 0), coalesce(sum(signups), 0) from daily_source_stats
			where project_id = $1 and source = $2 and day between $3 and $4`, projectID, m.Source, w.from, w.to).Scan(&sessions, &signups)
		return ratio(signups, sessions), sessions, err
	}
	return 0, 0, fmt.Errorf("unknown metric %q", m.Type)
}

type pgxQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func page(route string) string {
	if route == "" || route == "/" {
		return "the home page (/)"
	}
	return "the " + route + " page"
}

// Detect runs every detector for one project over the last 7 full days and
// returns at most MaxInsights, biggest first.
func Detect(ctx context.Context, q pgxQuerier, projectID int64, now time.Time) ([]Insight, error) {
	w := lastDays(now, 7)
	var out []Insight

	// 1. Sign-up or login errors: fixing them beats any growth idea.
	rows, err := q.Query(ctx, `select title, route, user_count from issues
		where project_id = $1 and status not in ('fixed', 'ignored')
		  and (signature like '%/auth/v1/signup%' or signature like '%accounts:signUp%' or route ~* '(sign-?up|register)')
		order by user_count desc limit 1`, projectID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var title, route string
		var users int64
		if err := rows.Scan(&title, &route, &users); err != nil {
			rows.Close()
			return nil, err
		}
		rate, visitors, _ := Measure(ctx, q, projectID, Metric{Type: "signup_rate"}, w)
		out = append(out, Insight{
			Detector: "signup_errors", Title: "People hit an error while signing up",
			Evidence:   fmt.Sprintf("%d people ran into “%s” on %s in the last weeks; your sign-up rate was %s of %d visitors in the last 7 days.", users, title, page(route), pct(rate), visitors),
			Hypothesis: "Fixing this error should let more of the people who try to sign up actually finish.",
			FixPrompt:  fmt.Sprintf("Fix the sign-up error “%s” first: open it in Fix Assistant and paste its fix prompt into your AI agent.", title),
			HowToCheck: "After you publish the fix, try signing up yourself with a new email address; it should work without errors.",
			// Pinned first: a broken sign-up blocks exactly what the founder wants.
			Metric: Metric{Type: "signup_rate", Better: "higher"}, Baseline: rate, weight: math.Inf(1),
		})
	}
	rows.Close()

	// 2. Funnel steps that lose most people.
	frows, err := q.Query(ctx, `select id, name, steps from funnels where project_id = $1`, projectID)
	if err != nil {
		return nil, err
	}
	type fn struct {
		id    int64
		name  string
		steps []Step
	}
	var funnels []fn
	for frows.Next() {
		var f fn
		var raw []byte
		if err := frows.Scan(&f.id, &f.name, &raw); err != nil {
			frows.Close()
			return nil, err
		}
		if f.steps, err = ParseSteps(raw); err == nil {
			funnels = append(funnels, f)
		}
	}
	frows.Close()
	for _, f := range funnels {
		for i := 1; i < len(f.steps); i++ {
			m := Metric{Type: "funnel_step", Funnel: f.id, Step: i, Better: "higher"}
			conv, prev, err := Measure(ctx, q, projectID, m, w)
			if err != nil {
				return nil, err
			}
			if prev < minSessions || 1-conv < funnelDropShare {
				continue
			}
			from, to := f.steps[i-1], f.steps[i]
			where := "between these two steps"
			if from.Kind == "page" {
				where = "on " + page(from.Match)
			}
			out = append(out, Insight{
				Detector: "funnel_drop", Title: fmt.Sprintf("Most people stop at step %d of “%s”", i+1, f.name),
				Evidence:   fmt.Sprintf("Only %s of %d sessions went from “%s” to “%s” in the last 7 days (%s left %s).", pct(conv), prev, stepText(from), stepText(to), pct(1-conv), where),
				Hypothesis: "If the next step is easier to see and do, more people may continue. This is a hypothesis to test, not a promise.",
				FixPrompt:  fmt.Sprintf("On %s, make the next step (“%s”) the obvious thing to do: one clear, prominent button for it, visible without scrolling on phones and computers, with a short line saying what people get. Don't change other pages or the sign-up logic.", pageOrHere(from), stepText(to)),
				HowToCheck: fmt.Sprintf("Open %s on a phone-sized screen: the button for “%s” should be visible right away.", pageOrHere(from), stepText(to)),
				Metric:     m, Baseline: conv, weight: float64(prev) * (1 - conv),
			})
		}
	}

	// 3 + 4. Pages: phones leaving much more than computers; slow pages.
	prows, err := q.Query(ctx, `select route,
		  coalesce(sum(sessions) filter (where device = 'mobile'), 0), coalesce(sum(exits) filter (where device = 'mobile'), 0),
		  coalesce(sum(sessions) filter (where device = 'desktop'), 0), coalesce(sum(exits) filter (where device = 'desktop'), 0),
		  coalesce(sum(views), 0), coalesce(sum(load_ms_sum), 0), coalesce(sum(load_ms_count), 0)
		from daily_page_stats where project_id = $1 and day between $2 and $3 group by route`, projectID, w.from, w.to)
	if err != nil {
		return nil, err
	}
	for prows.Next() {
		var route string
		var ms, me, ds, de, views, loadSum, loadN int64
		if err := prows.Scan(&route, &ms, &me, &ds, &de, &views, &loadSum, &loadN); err != nil {
			prows.Close()
			return nil, err
		}
		mRate, dRate := ratio(me, ms), ratio(de, ds)
		if ms >= minSessions && ds > 0 && mRate-dRate >= mobileExitGap {
			out = append(out, Insight{
				Detector: "mobile_exit", Title: fmt.Sprintf("Phone visitors leave %s much more often", page(route)),
				Evidence:   fmt.Sprintf("%s of phone sessions ended on %s, against %s on computers (%d phone sessions, last 7 days).", pct(mRate), page(route), pct(dRate), ms),
				Hypothesis: "The page may be harder to use on a small screen. Improving the phone layout may keep more phone visitors. A hypothesis to test.",
				FixPrompt:  fmt.Sprintf("Improve %s on phones (screens about 375px wide): the main content and main button must be visible without sideways scrolling, text readable without zooming, and buttons at least 44px tall. Change only the small-screen layout of this page.", page(route)),
				HowToCheck: fmt.Sprintf("Open %s in your browser's phone preview (375px wide): nothing should overflow and the main button should be easy to tap.", page(route)),
				Metric:     Metric{Type: "page_exit", Route: route, Device: "mobile", Better: "lower"}, Baseline: mRate, weight: float64(ms) * (mRate - dRate),
			})
		}
		avg := ratio(loadSum, loadN)
		if loadN >= minSessions && avg >= slowLoadMS {
			out = append(out, Insight{
				Detector: "slow_page", Title: fmt.Sprintf("%s is slow to load", strings.ToUpper(page(route)[:1])+page(route)[1:]),
				Evidence:   fmt.Sprintf("%s took %.1f seconds to load on average over %d views in the last 7 days.", strings.ToUpper(page(route)[:1])+page(route)[1:], avg/1000, loadN),
				Hypothesis: "Many visitors leave pages that take more than about 3 seconds. A faster page may keep more of them. A hypothesis to test.",
				FixPrompt:  fmt.Sprintf("Make %s load faster. Find the biggest cause first (large images, loading all data at once, or heavy libraries) and fix only that: compress and resize images, load below-the-fold content later, or load data in smaller pages.", page(route)),
				HowToCheck: fmt.Sprintf("Open %s with the browser's network tab: the page should finish loading in under 3 seconds.", page(route)),
				Metric:     Metric{Type: "page_load", Route: route, Better: "lower"}, Baseline: avg, weight: float64(loadN) * (avg/1000 - 2),
			})
		}
	}
	prows.Close()

	// 5. A traffic source that converts far below the rest.
	overall, _, err := Measure(ctx, q, projectID, Metric{Type: "signup_rate"}, w)
	if err != nil {
		return nil, err
	}
	srows, err := q.Query(ctx, `select source, coalesce(sum(sessions), 0), coalesce(sum(signups), 0) from daily_source_stats
		where project_id = $1 and day between $2 and $3 group by source`, projectID, w.from, w.to)
	if err != nil {
		return nil, err
	}
	for srows.Next() {
		var source string
		var sessions, signups int64
		if err := srows.Scan(&source, &sessions, &signups); err != nil {
			srows.Close()
			return nil, err
		}
		rate := ratio(signups, sessions)
		if source == "direct" || sessions < minSessions || overall <= 0 || rate >= overall*weakSourceFactor {
			continue
		}
		out = append(out, Insight{
			Detector: "weak_source", Title: fmt.Sprintf("Visitors from %s rarely sign up", source),
			Evidence:   fmt.Sprintf("%d sessions came from %s in the last 7 days and %s signed up, against %s overall.", sessions, source, pct(rate), pct(overall)),
			Hypothesis: "These visitors may expect something the page they land on doesn't show. Speaking to them directly may convert more. A hypothesis to test.",
			FixPrompt:  fmt.Sprintf("Visitors from %s sign up much less than others. On the page they land on, add one short section near the top that speaks to people coming from %s (what they get and a clear sign-up button). Don't change the page for other visitors' main flow.", source, source),
			HowToCheck: "Open the landing page: the new section and its sign-up button should be visible without scrolling.",
			Metric:     Metric{Type: "source_rate", Source: source, Better: "higher"}, Baseline: rate, weight: float64(sessions) * (overall - rate) * 10,
		})
	}
	srows.Close()

	sort.SliceStable(out, func(i, j int) bool { return out[i].weight > out[j].weight })
	if len(out) > MaxInsights {
		out = out[:MaxInsights]
	}
	return out, nil
}

func stepText(s Step) string {
	switch s.Kind {
	case "page":
		return "visit " + s.Match
	case "click":
		return "click " + s.Match
	case "event":
		return s.Match
	}
	return "sign up"
}

func pageOrHere(s Step) string {
	if s.Kind == "page" {
		return page(s.Match)
	}
	return "the page before it"
}

// WeekStart is the Monday of t's week (UTC date).
func WeekStart(t time.Time) time.Time {
	d := t.UTC().Truncate(24 * time.Hour)
	return d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7))
}

// Generate stores this week's insights for a project (idempotent per week,
// detector and title) and returns how many were new.
func Generate(ctx context.Context, pool *pgxpool.Pool, projectID int64, now time.Time) (int, error) {
	ins, err := Detect(ctx, pool, projectID, now)
	if err != nil {
		return 0, err
	}
	var release *string
	var r string
	if err := pool.QueryRow(ctx, `select release_id from releases where project_id = $1 order by first_seen_at desc limit 1`, projectID).Scan(&r); err == nil {
		release = &r
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	n := 0
	for _, in := range ins {
		m, _ := json.Marshal(in.Metric)
		tag, err := pool.Exec(ctx, `insert into growth_insights (project_id, week, detector, title, evidence, hypothesis, fix_prompt, how_to_check, metric, baseline, release_id)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) on conflict do nothing`,
			projectID, WeekStart(now), in.Detector, in.Title, in.Evidence, in.Hypothesis, in.FixPrompt, in.HowToCheck, m, roundTo(in.Baseline, 4), release)
		if err != nil {
			return n, err
		}
		n += int(tag.RowsAffected())
	}
	return n, nil
}

func roundTo(f float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(f*p) / p
}

// Outcome compares an after value with the baseline: 10% relative change in
// the better direction is "worked", in the other direction "worse".
func Outcome(m Metric, baseline, after float64) string {
	if baseline == 0 {
		switch {
		case after == 0:
			return "no_change"
		case m.Better == "higher":
			return "worked"
		default:
			return "worse"
		}
	}
	change := (after - baseline) / math.Abs(baseline)
	if m.Better == "lower" {
		change = -change
	}
	switch {
	case change >= changeThreshold:
		return "worked"
	case change <= -changeThreshold:
		return "worse"
	}
	return "no_change"
}

// Evaluate runs the before/after check (FDR): after the next detected
// release, re-measure over the same number of days and record the result.
func Evaluate(ctx context.Context, pool *pgxpool.Pool, now time.Time) (int, error) {
	rows, err := pool.Query(ctx, `select g.id, g.project_id, g.metric, g.baseline::float8, g.baseline_days, g.created_at
		from growth_insights g where g.outcome is null`)
	if err != nil {
		return 0, err
	}
	type pending struct {
		id, project int64
		metric      Metric
		baseline    float64
		days        int
		created     time.Time
	}
	var list []pending
	for rows.Next() {
		var p pending
		var raw []byte
		if err := rows.Scan(&p.id, &p.project, &raw, &p.baseline, &p.days, &p.created); err != nil {
			rows.Close()
			return 0, err
		}
		if json.Unmarshal(raw, &p.metric) == nil {
			list = append(list, p)
		}
	}
	rows.Close()
	done := 0
	for _, p := range list {
		var release string
		var firstSeen time.Time
		err := pool.QueryRow(ctx, `select release_id, first_seen_at from releases where project_id = $1 and first_seen_at > $2
			order by first_seen_at limit 1`, p.project, p.created).Scan(&release, &firstSeen)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // waiting for the founder's next release
		}
		if err != nil {
			return done, err
		}
		start := firstSeen.UTC().Truncate(24*time.Hour).AddDate(0, 0, 1) // first full day on the new release
		end := start.AddDate(0, 0, p.days-1)
		if !now.UTC().After(end.AddDate(0, 0, 1)) {
			continue // not enough days on the new release yet
		}
		after, _, err := Measure(ctx, pool, p.project, p.metric, window{from: start, to: end})
		if err != nil {
			return done, err
		}
		if _, err := pool.Exec(ctx, `update growth_insights set after_value = $2, after_release_id = $3, outcome = $4, outcome_source = 'implicit'
			where id = $1 and outcome is null`, p.id, roundTo(after, 4), release, Outcome(p.metric, p.baseline, after)); err != nil {
			return done, err
		}
		done++
	}
	return done, nil
}

// Package perf turns page-speed regressions into issues (kind "perf"):
// "Loading on /pricing got slower". It reads the vitals histograms
// (daily_vitals, see analytics.Vitals) and, for each page, metric (LCP, INP,
// CLS) and device class, compares the page's current period with the one
// before:
//
//   - by release, when the page started serving a new release (the SDK's
//     release id hashes the page's own scripts, so pages of one site can
//     differ) that has been live for MinReleaseAge: everything measured on
//     the new release against the two weeks of other releases before it;
//   - by date otherwise (a backend-only deploy keeps the release id): the
//     last 3 days against the 2 weeks before.
//
// A regression needs enough measurements on both sides, a worse rating
// (good → needs work → poor), a relative and an absolute jump, and to show
// on two runs in a row. It opens one issue per page and metric (the worse
// device), reopens a fixed one, and queues the usual analysis job, which
// explains perf issues with Explain instead of the error analyzer.
//
// Verification is by speed too: an open slowdown is fixed once data from
// after its fix prompt shows the page back at its earlier level.
package perf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/fixassist/internal/analytics"
	"github.com/yourname/fixassist/internal/grouping"
	"github.com/yourname/fixassist/internal/store/db"
	"github.com/yourname/fixassist/internal/verify"
)

// Kind is the issue kind for slowdowns.
const Kind = "perf"

// Rules are the detection thresholds.
type Rules struct {
	MinSamples    int64         // measurements per side, per device
	MinReleaseAge time.Duration // a release is judged once caches are warm
	Persist       time.Duration // a regression must still be there this much later
	Growth        float64       // and at least this many times the earlier p75
}

// DefaultRules: 50 measurements a side, a release live 12 hours, seen on
// two hourly runs, 25% slower.
var DefaultRules = Rules{MinSamples: 50, MinReleaseAge: 12 * time.Hour, Persist: 50 * time.Minute, Growth: 1.25}

// Metrics are the vitals that become issues, with the smallest change worth
// one (FCP and TTFB are shown on the Speed screen only).
var Metrics = map[string]float64{"LCP": 500, "INP": 100, "CLS": 0.05}

var devices = []string{"mobile", "desktop"}

// Enqueue queues the analysis job for an issue (jobargs.AnalyzeIssue) in tx.
type Enqueue func(ctx context.Context, tx pgx.Tx, issueID int64, reason string) error

// Stats says what one run did.
type Stats struct {
	Pending, Opened, Reopened, Updated, Fixed int
}

// cell is one day's histogram for a page, device, release and metric.
type cell struct {
	day     time.Time
	route   string
	device  string
	release string
	metric  string
	bucket  int
	n       int64
}

// window is one side of a comparison.
type window struct {
	hist    analytics.Histogram
	release string    // the new release, or "" when compared by date
	since   time.Time // when the period began
}

// Run detects regressions and verifies open slowdowns for every project
// with recent measurements.
func Run(ctx context.Context, pool *pgxpool.Pool, rules Rules, now time.Time, enqueue Enqueue) (Stats, error) {
	var st Stats
	rows, err := pool.Query(ctx, `select distinct project_id from daily_vitals where day >= $1`, day(now).AddDate(0, 0, -30))
	if err != nil {
		return st, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return st, err
	}
	// One project's trouble mustn't stop detection for the others.
	var errs []error
	for _, id := range ids {
		if err := runProject(ctx, pool, rules, now, id, enqueue, &st); err != nil {
			errs = append(errs, fmt.Errorf("project %d: %w", id, err))
		}
	}
	return st, errors.Join(errs...)
}

func day(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

// data is a project's last 30 days of histograms and its releases.
type data struct {
	cells    []cell
	releases map[string]time.Time // first seen
}

func load(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, projectID int64, now time.Time) (data, error) {
	d := data{releases: map[string]time.Time{}}
	rows, err := q.Query(ctx, `select day, route, device, release_id, metric, bucket, sum(n)::bigint from daily_vitals
		where project_id = $1 and day >= $2 and device in ('mobile', 'desktop') and metric in ('LCP', 'INP', 'CLS')
		group by 1, 2, 3, 4, 5, 6`, projectID, day(now).AddDate(0, 0, -30))
	if err != nil {
		return d, err
	}
	for rows.Next() {
		var c cell
		if err := rows.Scan(&c.day, &c.route, &c.device, &c.release, &c.metric, &c.bucket, &c.n); err != nil {
			rows.Close()
			return d, err
		}
		c.day = day(c.day)
		d.cells = append(d.cells, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return d, err
	}
	rows, err = q.Query(ctx, `select release_id, first_seen_at from releases where project_id = $1`, projectID)
	if err != nil {
		return d, err
	}
	for rows.Next() {
		var id string
		var t time.Time
		if err := rows.Scan(&id, &t); err != nil {
			rows.Close()
			return d, err
		}
		d.releases[id] = t
	}
	rows.Close()
	return d, rows.Err()
}

// periods splits a page's measurements on one device into the current
// period and the one before, per metric.
func (d data) periods(route, device string, rules Rules, now time.Time) (cur, base map[string]window) {
	today := day(now)
	// The release this page is serving now: the most measured in the last
	// two days (after a rollback that's the old one again).
	recent := map[string]int64{}
	for _, c := range d.cells {
		if c.route == route && c.device == device && !c.day.Before(today.AddDate(0, 0, -1)) {
			recent[c.release] += c.n
		}
	}
	var release string
	var most int64
	for r, n := range recent {
		if n > most || (n == most && r < release) {
			release, most = r, n
		}
	}
	first, known := d.releases[release]
	cur, base = map[string]window{}, map[string]window{}
	for m := range Metrics {
		cur[m] = window{hist: analytics.Histogram{}}
		base[m] = window{hist: analytics.Histogram{}}
	}
	if known && now.Sub(first) < rules.MinReleaseAge {
		// Just deployed: caches are cold and the first visits are slow. Don't
		// judge this page (by release or by date) until it has settled.
		return cur, base
	}
	byRelease := release != "" && known && first.After(now.AddDate(0, 0, -14))
	// The baseline is the release this page served just before: an earlier
	// slow release mustn't hide a new slowdown. If that one has too few
	// measurements here, all earlier releases of the two weeks before count.
	prev := ""
	if byRelease {
		var prevFirst time.Time
		perRelease := map[string]int64{}
		for _, c := range d.cells {
			if c.route == route && c.device == device && c.release != release {
				perRelease[c.release] += c.n
			}
		}
		for r := range perRelease {
			if f, ok := d.releases[r]; ok && f.Before(first) && f.After(prevFirst) {
				prev, prevFirst = r, f
			}
		}
		if perRelease[prev] < rules.MinSamples {
			prev = ""
		}
	}
	for _, c := range d.cells {
		if c.route != route || c.device != device {
			continue
		}
		var into map[string]window
		switch {
		case byRelease && c.release == release:
			into = cur
		case byRelease && prev != "" && c.release == prev:
			into = base
		case byRelease && prev == "" && !c.day.Before(day(first).AddDate(0, 0, -14)):
			into = base
		case !byRelease && !c.day.Before(today.AddDate(0, 0, -2)):
			into = cur
		case !byRelease && !c.day.Before(today.AddDate(0, 0, -16)):
			into = base
		default:
			continue
		}
		into[c.metric].hist[c.bucket] += c.n
	}
	since := today.AddDate(0, 0, -2)
	rel := ""
	if byRelease {
		since, rel = first, release
	}
	for m, w := range cur {
		w.since, w.release = since, rel
		cur[m] = w
	}
	return cur, base
}

// rank orders ratings: good 0, needs work 1, poor 2.
func rank(metric string, x float64) int {
	v, _ := analytics.VitalByName(metric)
	switch v.Rating(x) {
	case "good":
		return 0
	case "needs-improvement":
		return 1
	}
	return 2
}

// regressed says whether cur is clearly slower than base.
func regressed(metric string, base, cur float64, rules Rules) bool {
	return rank(metric, cur) > rank(metric, base) && rank(metric, cur) > 0 &&
		cur >= base*rules.Growth && cur-base >= Metrics[metric]
}

// finding is a regression on a page and metric (the worse device).
type finding struct {
	route, metric, device string
	base, cur             float64
	baseN, curN           int64
	slow                  int64 // current measurements over "good"
	w                     window
}

func runProject(ctx context.Context, pool *pgxpool.Pool, rules Rules, now time.Time, projectID int64, enqueue Enqueue, st *Stats) error {
	d, err := load(ctx, pool, projectID, now)
	if err != nil {
		return err
	}
	routes := map[string]bool{}
	for _, c := range d.cells {
		routes[c.route] = true
	}
	found := map[[2]string]finding{}
	measured := map[[2]string]bool{}   // enough data to say it isn't regressing
	current := map[[3]string]float64{} // route, device, metric → current p75 (for verification)
	currentSince := map[[3]string]window{}
	for route := range routes {
		for _, dev := range devices {
			cur, base := d.periods(route, dev, rules, now)
			for m := range Metrics {
				cw, bw := cur[m], base[m]
				if cw.hist.N() >= rules.MinSamples {
					current[[3]string{route, dev, m}] = cw.hist.Percentile(m, 0.75)
					currentSince[[3]string{route, dev, m}] = cw
				}
				if cw.hist.N() < rules.MinSamples || bw.hist.N() < rules.MinSamples {
					continue
				}
				b, c := bw.hist.Percentile(m, 0.75), cw.hist.Percentile(m, 0.75)
				k := [2]string{route, m}
				measured[k] = true
				if !regressed(m, b, c, rules) {
					continue
				}
				f := finding{route: route, metric: m, device: dev, base: b, cur: c, baseN: bw.hist.N(), curN: cw.hist.N(), slow: slowCount(m, cw.hist), w: cw}
				if old, ok := found[k]; !ok || c/math.Max(b, 1e-9) > old.cur/math.Max(old.base, 1e-9) {
					found[k] = f
				}
			}
		}
	}

	q := db.New(pool)
	var keyPages []string
	if err := pool.QueryRow(ctx, `select coalesce(key_pages, '{}') from projects where id = $1`, projectID).Scan(&keyPages); err != nil {
		return err
	}
	// Candidates for pages that are no longer slow, or no longer measured.
	keep := []string{}
	for k := range found {
		keep = append(keep, k[0]+"|"+k[1])
	}
	if _, err := pool.Exec(ctx, `delete from perf_candidates where project_id = $1 and route || '|' || metric <> all($2::text[])`, projectID, keep); err != nil {
		return err
	}
	for k := range measured {
		if _, ok := found[k]; !ok {
			if err := q.DropPerfCandidate(ctx, db.DropPerfCandidateParams{ProjectID: projectID, Route: k[0], Metric: k[1]}); err != nil {
				return err
			}
		}
	}
	for _, f := range found {
		first, err := q.SeePerfCandidate(ctx, db.SeePerfCandidateParams{ProjectID: projectID, Route: f.route, Metric: f.metric, Now: ts(now)})
		if err != nil {
			return err
		}
		if now.Sub(first.Time) < rules.Persist {
			st.Pending++
			continue
		}
		if err := open(ctx, pool, projectID, f, now, keyPages, enqueue, st); err != nil {
			return err
		}
	}
	return verifyProject(ctx, pool, projectID, rules, now, current, currentSince, st)
}

// impactOf puts a slowdown on the errors' scale (grouping.Impact: people
// in a day × 3 on key pages): slow visits per day × the page's weight.
func impactOf(f finding, now time.Time, keyPages []string) float64 {
	days := math.Max(1, math.Ceil(now.Sub(f.w.since).Hours()/24))
	return math.Max(1, float64(f.slow)/days) * grouping.RouteWeight(f.route, keyPages)
}

// slowCount is how many measurements in h are over the "good" threshold.
func slowCount(metric string, h analytics.Histogram) int64 {
	v, _ := analytics.VitalByName(metric)
	edges := analytics.VitalEdges(metric)
	var n int64
	for b, c := range h {
		if b >= 1 && b-1 < len(edges) && edges[b-1] >= v.Good {
			n += c
		}
	}
	return n
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func numeric(f float64) pgtype.Numeric {
	var n pgtype.Numeric
	_ = n.Scan(fmt.Sprintf("%.2f", f))
	return n
}

// Title is a slowdown's issue title.
func Title(metric, route string) string {
	v, _ := analytics.VitalByName(metric)
	what := map[string]string{"LCP": "Loading", "INP": "Responding to taps and clicks", "CLS": "Layout jumping"}[metric]
	if what == "" {
		what = v.Label
	}
	verb := "got slower"
	if metric == "CLS" {
		verb = "got worse"
	}
	return fmt.Sprintf("%s %s on %s", what, verb, route)
}

// open creates, reopens or refreshes the page's slowdown issue.
func open(ctx context.Context, pool *pgxpool.Pool, projectID int64, f finding, now time.Time, keyPages []string, enqueue Enqueue, st *Stats) error {
	culprit, err := culprits(ctx, pool, projectID, f)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		fp := Kind + ":" + f.metric + ":" + f.route
		title := Title(f.metric, f.route)
		impact := numeric(impactOf(f, now, keyPages))
		is, err := q.PerfIssueByFingerprint(ctx, db.PerfIssueByFingerprintParams{ProjectID: projectID, Fingerprint: fp})
		reason := ""
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			is, err = q.InsertPerfIssue(ctx, db.InsertPerfIssueParams{ProjectID: projectID, Fingerprint: fp, Title: title,
				Route: pgtype.Text{String: f.route, Valid: true}, Now: ts(now), SlowVisits: f.slow, Impact: impact})
			if err != nil {
				return err
			}
			reason = "threshold"
			st.Opened++
		case err != nil:
			return err
		case is.Status == "ignored":
			return nil // the founder doesn't want it
		case is.Status == "fixed" && !f.w.since.After(is.FixedAt.Time):
			// Marked fixed (by the founder, or before this period's data):
			// only measurements taken after that can say it came back.
			return nil
		case is.Status == "fixed":
			if is, err = q.ReopenPerfIssue(ctx, db.ReopenPerfIssueParams{Now: ts(now), Title: title, SlowVisits: f.slow, Impact: impact, ID: is.ID}); err != nil {
				return err
			}
			// The fix didn't hold (on the attempt being watched, if any).
			if err := verify.Record(ctx, q, is.ID, verify.Failed, verify.Implicit, f.w.release, 0); err != nil {
				return err
			}
			reason = "regression"
			st.Reopened++
		default:
			if err := q.TouchPerfIssue(ctx, db.TouchPerfIssueParams{Now: ts(now), Title: title, SlowVisits: f.slow, Impact: impact, ID: is.ID}); err != nil {
				return err
			}
			// Keep the speed it was before it slowed down as the target: a
			// partial fix followed by a new slowdown mustn't lower the bar.
			if old, err := q.GetPerfRegression(ctx, is.ID); err == nil {
				f.base, f.baseN = old.BaselineP75, old.BaselineN
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			st.Updated++
		}
		breakdown, _ := json.Marshal(culprit.breakdown)
		if err := q.UpsertPerfRegression(ctx, db.UpsertPerfRegressionParams{
			IssueID: is.ID, ProjectID: projectID, Route: f.route, Metric: f.metric, Device: f.device,
			BaselineP75: f.base, BaselineN: f.baseN, CurrentP75: f.cur, CurrentN: f.curN,
			CurrentRelease: f.w.release, Since: ts(f.w.since), Element: culprit.element, Source: culprit.source,
			Breakdown: breakdown, Now: ts(now),
		}); err != nil {
			return err
		}
		// The candidate stays while the regression lasts (later runs refresh the
		// issue); it's dropped once the page is measured fast again.
		if reason != "" && enqueue != nil {
			return enqueue(ctx, tx, is.ID, reason)
		}
		return nil
	})
}

type culprit struct {
	element, source string
	breakdown       map[string]float64
}

// culprits reads the slow measurements of the current period from the raw
// events (30 days): the most common element and resource, and the average
// of each phase. Visitors' browsers report these: data, never instructions.
func culprits(ctx context.Context, pool *pgxpool.Pool, projectID int64, f finding) (culprit, error) {
	v, _ := analytics.VitalByName(f.metric)
	c := culprit{breakdown: map[string]float64{}}
	const slow = `from analytics_events
		where project_id = $1 and type = 'vital' and route = $2 and name = $3 and device = $4 and ts >= $5
		  and ($6 = '' or props->>'rel' = $6)
		  and jsonb_typeof(props->'v') = 'number' and (props->>'v')::float8 > $7`
	err := pool.QueryRow(ctx, `select coalesce(props->>'el', ''), coalesce(props->>'src', '') `+slow+`
		and (coalesce(props->>'el', '') <> '' or coalesce(props->>'src', '') <> '')
		group by 1, 2 order by count(*) desc, 1, 2 limit 1`,
		projectID, f.route, f.metric, f.device, f.w.since, f.w.release, v.Good).Scan(&c.element, &c.source)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return c, err
	}
	phases := map[string][]string{"LCP": {"ttfb", "rld", "rlt", "erd"}, "INP": {"id", "pd", "prd"}}[f.metric]
	for _, p := range phases {
		var avg pgtype.Float8
		if err := pool.QueryRow(ctx, `select avg((props->>'`+p+`')::float8) `+slow+` and jsonb_typeof(props->'`+p+`') = 'number'`,
			projectID, f.route, f.metric, f.device, f.w.since, f.w.release, v.Good).Scan(&avg); err != nil {
			return c, err
		}
		if avg.Valid {
			c.breakdown[p] = math.Round(avg.Float64)
		}
	}
	return c, nil
}

// verifyProject marks a slowdown fixed when data from after its fix prompt
// shows the page back at its earlier level (or rated good).
func verifyProject(ctx context.Context, pool *pgxpool.Pool, projectID int64, rules Rules, now time.Time,
	current map[[3]string]float64, windows map[[3]string]window, st *Stats) error {
	q := db.New(pool)
	list, err := q.PerfIssuesToVerify(ctx)
	if err != nil {
		return err
	}
	for _, is := range list {
		if is.ProjectID != projectID || !is.SuggestedAt.Valid {
			continue
		}
		k := [3]string{is.Route, is.Device, is.Metric}
		c, ok := current[k]
		w := windows[k]
		// Only data from after the prompt counts: a new release first seen
		// since, or a date window that starts after it.
		if !ok || !w.since.After(is.SuggestedAt.Time) {
			continue
		}
		if rank(is.Metric, c) > rank(is.Metric, is.BaselineP75) && c-is.BaselineP75 >= Metrics[is.Metric]/2 {
			continue // still slow
		}
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			tq := db.New(tx)
			n, err := tq.MarkPerfFixed(ctx, db.MarkPerfFixedParams{Now: ts(now), Release: w.release, ID: is.ID})
			if err != nil || n == 0 {
				return err
			}
			st.Fixed++
			return verify.Record(ctx, tq, is.ID, verify.Worked, verify.Implicit, w.release, 0)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

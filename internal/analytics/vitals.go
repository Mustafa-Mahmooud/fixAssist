package analytics

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Page speed: Core Web Vitals measured in visitors' browsers by the SDK
// (Google's web-vitals), stored as 'vital' analytics events and rolled up
// into daily histograms (daily_vitals). Percentiles come from the buckets,
// so any range of days, devices or releases combines correctly.

// Vital is one Core Web Vital.
type Vital struct {
	Name  string
	Label string  // what founders see
	Good  float64 // at or under: good
	Poor  float64 // over: poor (between: needs improvement)
	Unit  string  // "ms" or "" (CLS)
}

// Vitals are the metrics the SDK sends, with Google's thresholds.
var Vitals = []Vital{
	{Name: "LCP", Label: "Loading", Good: 2500, Poor: 4000, Unit: "ms"},
	{Name: "INP", Label: "Responsiveness", Good: 200, Poor: 500, Unit: "ms"},
	{Name: "CLS", Label: "Layout shift", Good: 0.1, Poor: 0.25},
	{Name: "FCP", Label: "First paint", Good: 1800, Poor: 3000, Unit: "ms"},
	{Name: "TTFB", Label: "Server response", Good: 800, Poor: 1800, Unit: "ms"},
}

// VitalByName finds a metric's definition.
func VitalByName(name string) (Vital, bool) {
	for _, v := range Vitals {
		if v.Name == name {
			return v, true
		}
	}
	return Vital{}, false
}

// Bucket edges. Every threshold above is an edge, so each bucket lies wholly
// on one side of it and ratings from buckets are exact. Bucket i (from
// Postgres' width_bucket) holds values in [edges[i-1], edges[i]); the last
// one holds everything from the last edge up.
var (
	msEdges  = []float64{0, 50, 100, 150, 200, 250, 300, 400, 500, 600, 700, 800, 900, 1000, 1200, 1400, 1600, 1800, 2000, 2250, 2500, 2750, 3000, 3500, 4000, 4500, 5000, 6000, 7000, 8000, 10000, 12000, 15000, 20000, 30000, 60000}
	clsEdges = []float64{0, 0.01, 0.025, 0.05, 0.075, 0.1, 0.125, 0.15, 0.2, 0.25, 0.3, 0.4, 0.5, 0.75, 1, 2, 5}
)

// VitalEdges are a metric's bucket edges.
func VitalEdges(metric string) []float64 {
	if metric == "CLS" {
		return clsEdges
	}
	return msEdges
}

func sqlArray(xs []float64) string {
	s := make([]string, len(xs))
	for i, x := range xs {
		s[i] = fmt.Sprint(x)
	}
	return "array[" + strings.Join(s, ",") + "]::float8[]"
}

// vnum is a measurement's value in SQL: null unless props.v is a JSON
// number. Founders' own events can carry a "v" prop too, and Postgres may
// cast before it checks type = 'vital', so a bare cast could fail a whole
// rollup.
const vnum = `(case when jsonb_typeof(props->'v') = 'number' then (props->>'v')::float8 end)`

// The vitals rollup for one UTC day ($1 day, $2 start, $3 end), run with the
// others. Values were checked at ingest (numeric, in range).
func init() {
	dayStatements = append(dayStatements,
		`delete from daily_vitals where day = $1`,
		`insert into daily_vitals (project_id, day, route, device, release_id, metric, bucket, n)
		 select project_id, $1::date, coalesce(route, ''), coalesce(device, 'unknown'), left(coalesce(props->>'rel', ''), 100), name,
		   width_bucket(`+vnum+`, case when name = 'CLS' then `+sqlArray(clsEdges)+` else `+sqlArray(msEdges)+` end),
		   count(*)
		 from analytics_events
		 where type = 'vital' and name in ('LCP', 'INP', 'CLS', 'FCP', 'TTFB') and jsonb_typeof(props->'v') = 'number' and ts >= $2 and ts < $3
		 group by 1, 3, 4, 5, 6, 7`)
}

// Histogram is measurement counts by bucket (width_bucket numbering).
type Histogram map[int]int64

// N is how many measurements there are.
func (h Histogram) N() int64 {
	var n int64
	for _, c := range h {
		n += c
	}
	return n
}

// Percentile estimates the p-th percentile (0-1) by interpolating within
// the bucket that holds it; 0 when empty.
func (h Histogram) Percentile(metric string, p float64) float64 {
	n := h.N()
	if n == 0 {
		return 0
	}
	edges := VitalEdges(metric)
	buckets := make([]int, 0, len(h))
	for b := range h {
		buckets = append(buckets, b)
	}
	sort.Ints(buckets)
	rank := p * float64(n)
	var seen float64
	for _, b := range buckets {
		c := float64(h[b])
		if seen+c >= rank {
			lo, hi := bucketRange(edges, b)
			return lo + (hi-lo)*((rank-seen)/c)
		}
		seen += c
	}
	lo, _ := bucketRange(edges, buckets[len(buckets)-1])
	return lo
}

// bucketRange is bucket b's values; the last bucket is open-ended, so its
// "top" is taken as twice its floor.
func bucketRange(edges []float64, b int) (float64, float64) {
	switch {
	case b <= 0:
		return 0, edges[0]
	case b >= len(edges):
		last := edges[len(edges)-1]
		return last, last * 2
	}
	return edges[b-1], edges[b]
}

// Rating is "good", "needs-improvement" or "poor" for a value.
func (v Vital) Rating(x float64) string {
	switch {
	case x <= v.Good:
		return "good"
	case x <= v.Poor:
		return "needs-improvement"
	}
	return "poor"
}

// MinVitalSamples is how many measurements a percentile needs before the
// dashboard shows it; fewer would be noise.
const MinVitalSamples = 20

// VitalStat is one metric's p75 for a page.
type VitalStat struct {
	P75    float64
	N      int64
	Rating string // "" when there are too few measurements
}

// PageSpeed is one page's vitals over a period.
type PageSpeed struct {
	Route string
	Stats map[string]VitalStat // by metric name
	// Most common element and resource or script behind slow measurements
	// (over the "good" threshold), from the raw events' attribution.
	Culprits map[string]Culprit
}

// Culprit is what slow measurements of a metric most often point at.
type Culprit struct {
	Element, Source string
	Share           float64 // of the slow measurements
}

// Worst is the page's worst rated metric among LCP, INP and CLS: "poor",
// "needs-improvement", "good" or "".
func (p PageSpeed) Worst() string {
	rank := map[string]int{"": 0, "good": 1, "needs-improvement": 2, "poor": 3}
	w := ""
	for _, m := range []string{"LCP", "INP", "CLS"} {
		if r := p.Stats[m].Rating; rank[r] > rank[w] {
			w = r
		}
	}
	return w
}

// stats turns histograms into p75s and ratings.
func stats(h map[string]Histogram) map[string]VitalStat {
	out := map[string]VitalStat{}
	for _, v := range Vitals {
		hist := h[v.Name]
		s := VitalStat{N: hist.N()}
		if s.N >= MinVitalSamples {
			s.P75 = hist.Percentile(v.Name, 0.75)
			s.Rating = v.Rating(s.P75)
		}
		out[v.Name] = s
	}
	return out
}

// Speed is the Speed screen: the site's vitals and each page's, over the
// days [from, to], for device ("" = all, "mobile", "desktop", "tablet").
// Pages come worst first.
func Speed(ctx context.Context, q pgxQuerier, projectID int64, from, to time.Time, device string) (site PageSpeed, pages []PageSpeed, err error) {
	rows, err := q.Query(ctx, `select route, metric, bucket, sum(n)::bigint from daily_vitals
		where project_id = $1 and day between $2 and $3 and ($4 = '' or device = $4)
		group by route, metric, bucket`, projectID, from, to, device)
	if err != nil {
		return site, nil, err
	}
	byRoute := map[string]map[string]Histogram{}
	all := map[string]Histogram{}
	for rows.Next() {
		var route, metric string
		var bucket int
		var n int64
		if err := rows.Scan(&route, &metric, &bucket, &n); err != nil {
			rows.Close()
			return site, nil, err
		}
		if byRoute[route] == nil {
			byRoute[route] = map[string]Histogram{}
		}
		if byRoute[route][metric] == nil {
			byRoute[route][metric] = Histogram{}
		}
		if all[metric] == nil {
			all[metric] = Histogram{}
		}
		byRoute[route][metric][bucket] += n
		all[metric][bucket] += n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return site, nil, err
	}
	culprits, err := slowCulprits(ctx, q, projectID, from, to.AddDate(0, 0, 1), device)
	if err != nil {
		return site, nil, err
	}
	site = PageSpeed{Stats: stats(all)}
	for route, h := range byRoute {
		pages = append(pages, PageSpeed{Route: route, Stats: stats(h), Culprits: culprits[route]})
	}
	rank := map[string]int{"poor": 0, "needs-improvement": 1, "good": 2, "": 3}
	sort.Slice(pages, func(i, j int) bool {
		a, b := pages[i], pages[j]
		if rank[a.Worst()] != rank[b.Worst()] {
			return rank[a.Worst()] < rank[b.Worst()]
		}
		if a.Stats["LCP"].N != b.Stats["LCP"].N {
			return a.Stats["LCP"].N > b.Stats["LCP"].N
		}
		return a.Route < b.Route
	})
	return site, pages, nil
}

// slowCulprits finds, per page and metric, the element and resource most
// often behind measurements over the "good" threshold in the raw events
// (kept 30 days). Visitors' browsers report these, so they're shown as
// data, never followed as instructions.
func slowCulprits(ctx context.Context, q pgxQuerier, projectID int64, from, until time.Time, device string) (map[string]map[string]Culprit, error) {
	rows, err := q.Query(ctx, `with slow as (
		  select coalesce(route, '') as route, name, coalesce(props->>'el', '') as el, coalesce(props->>'src', '') as src
		  from analytics_events
		  where project_id = $1 and type = 'vital' and ts >= $2 and ts < $3 and ($4 = '' or device = $4)
		    and ((name = 'LCP' and `+vnum+` > 2500) or (name = 'INP' and `+vnum+` > 200)
		      or (name = 'CLS' and `+vnum+` > 0.1))
		), counted as (
		  select route, name, el, src, count(*) as c, sum(count(*)) over (partition by route, name) as total
		  from slow where el <> '' or src <> '' group by route, name, el, src
		)
		select distinct on (route, name) route, name, el, src, c::float8 / total
		from counted order by route, name, c desc, el, src`, projectID, from, until, device)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]Culprit{}
	for rows.Next() {
		var route, name string
		var c Culprit
		if err := rows.Scan(&route, &name, &c.Element, &c.Source, &c.Share); err != nil {
			return nil, err
		}
		if out[route] == nil {
			out[route] = map[string]Culprit{}
		}
		out[route][name] = c
	}
	return out, rows.Err()
}

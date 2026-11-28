package analytics_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/yourname/fixassist/internal/analytics"
)

func TestHistogramPercentile(t *testing.T) {
	lcp, _ := analytics.VitalByName("LCP")
	cls, _ := analytics.VitalByName("CLS")
	// width_bucket numbering (how many edges are <= the value): LCP 1500 ms
	// is in [1400, 1600), bucket 16; 5200 ms in [5000, 6000), bucket 27.
	h := analytics.Histogram{16: 30, 27: 10} // 30 fast loads, 10 at 5-6 s
	if p := h.Percentile("LCP", 0.75); p != 1600 {
		t.Errorf("p75 = %v, want 1600 (top of the bucket holding the 30th)", p)
	}
	if p := h.Percentile("LCP", 0.9); p < 5000 || p > 6000 {
		t.Errorf("p90 = %v, want within the 5-6 s bucket", p)
	}
	if h.N() != 40 || (analytics.Histogram{}).Percentile("LCP", 0.75) != 0 {
		t.Error("counts")
	}
	for _, c := range []struct {
		v    analytics.Vital
		x    float64
		want string
	}{{lcp, 2500, "good"}, {lcp, 2501, "needs-improvement"}, {lcp, 4000, "needs-improvement"}, {lcp, 4001, "poor"},
		{cls, 0.1, "good"}, {cls, 0.2, "needs-improvement"}, {cls, 0.3, "poor"}} {
		if got := c.v.Rating(c.x); got != c.want {
			t.Errorf("%s %v = %s, want %s", c.v.Name, c.x, got, c.want)
		}
	}
	// Every threshold is a bucket edge, so a bucket never straddles a rating.
	for _, v := range analytics.Vitals {
		edges := analytics.VitalEdges(v.Name)
		for _, th := range []float64{v.Good, v.Poor} {
			found := false
			for _, e := range edges {
				found = found || math.Abs(e-th) < 1e-9
			}
			if !found {
				t.Errorf("%s threshold %v isn't a bucket edge", v.Name, th)
			}
		}
	}
}

func TestVitalsRollupAndSpeed(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	vital := func(path, name string, v float64, extra string) string {
		return fmt.Sprintf(`{"type":"vital","path":%q,"name":%q,"props":{"v":%v,"rel":"r1","nav":"navigate"%s}}`, path, name, v, extra)
	}
	batch := func(sid, device string, events []string) {
		for len(events) > 0 {
			n := min(len(events), 50)
			f.visit("UA-"+sid, sid, device, nil, strings.Join(events[:n], ","))
			events = events[n:]
		}
	}
	// /pricing on computers: 30 quick loads, 10 slow ones behind a hero
	// image; slow taps on the buy button.
	var ev []string
	for i := 0; i < 30; i++ {
		ev = append(ev, vital("/pricing", "LCP", 1500, ""))
		ev = append(ev, vital("/pricing", "CLS", 0.02, ""))
	}
	for i := 0; i < 10; i++ {
		ev = append(ev, vital("/pricing", "LCP", 5200, `,"el":"main>img.hero","src":"https://cdn.example.com/hero.png?"`))
	}
	for i := 0; i < 25; i++ {
		ev = append(ev, vital("/pricing", "INP", 450, `,"el":"button#buy","src":"https://app.example.com/assets/index.js"`))
	}
	batch("s1", "desktop", ev)
	// The home page: only a few measurements, too few to rate.
	batch("s2", "mobile", []string{vital("/", "LCP", 900, ""), vital("/", "LCP", 1100, "")})
	// Junk the server must drop: unknown metric, negative, out of range,
	// not a number. And a founder's own event with a "v" prop that isn't a
	// number, which must not break the rollup.
	batch("s3", "desktop", []string{
		vital("/x", "FID", 10, ""), vital("/x", "LCP", -1, ""), vital("/x", "CLS", 50, ""), vital("/x", "INP", 999999, ""),
		`{"type":"vital","path":"/x","name":"LCP","props":{"v":"slow"}}`,
		`{"type":"custom","path":"/x","name":"rated","props":{"v":"abc"}}`,
	})
	if n := f.val("select count(*)::text from analytics_events where type = 'vital'"); n != "97" {
		t.Fatalf("vitals stored = %s, want 97 (junk dropped)", n)
	}

	if err := analytics.RollupDay(ctx, f.pool, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := analytics.RollupDay(ctx, f.pool, time.Now()); err != nil {
		t.Fatal(err) // idempotent
	}
	if s := f.val("select sum(n)::text from daily_vitals where route = '/pricing' and metric = 'LCP' and release_id = 'r1'"); s != "40" {
		t.Errorf("LCP measurements rolled up = %s, want 40", s)
	}

	day := time.Now().UTC()
	site, pages, err := analytics.Speed(ctx, f.pool, f.p.ID, day.AddDate(0, 0, -6), day, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 || pages[0].Route != "/pricing" {
		t.Fatalf("pages = %+v", pages)
	}
	pr := pages[0]
	if s := pr.Stats["LCP"]; s.N != 40 || s.P75 != 1600 || s.Rating != "good" {
		t.Errorf("LCP = %+v (p75 of 30 x 1.5 s and 10 x 5.2 s)", s)
	}
	if s := pr.Stats["INP"]; s.Rating != "needs-improvement" || pr.Worst() != "needs-improvement" {
		t.Errorf("INP = %+v, worst %q", s, pr.Worst())
	}
	if c := pr.Culprits["INP"]; c.Element != "button#buy" || !strings.HasSuffix(c.Source, "index.js") || c.Share != 1 {
		t.Errorf("INP culprit = %+v", c)
	}
	if c := pr.Culprits["LCP"]; c.Element != "main>img.hero" || strings.Contains(c.Source, "?x") {
		t.Errorf("LCP culprit = %+v", c)
	}
	if home := pages[1]; home.Stats["LCP"].N != 2 || home.Stats["LCP"].Rating != "" {
		t.Errorf("home = %+v (too few to rate)", home.Stats["LCP"])
	}
	if site.Stats["LCP"].N != 42 {
		t.Errorf("site LCP n = %d", site.Stats["LCP"].N)
	}
	// Device filter: phones only saw the home page.
	_, phone, err := analytics.Speed(ctx, f.pool, f.p.ID, day.AddDate(0, 0, -6), day, "mobile")
	if err != nil || len(phone) != 1 || phone[0].Route != "/" {
		t.Errorf("mobile = %+v %v", phone, err)
	}
}

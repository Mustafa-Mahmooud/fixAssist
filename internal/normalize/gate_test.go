package normalize_test

// Phase 1 gate: runs against the envelopes the spike harness captured from
// the pinned Sentry SDK + add-on (internal/ingest/testdata). Regenerate with
// spike/harness/run.mjs; see README.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/yourname/fixassist/internal/ingest"
	"github.com/yourname/fixassist/internal/normalize"
)

const testdata = "../ingest/testdata"

type releasesFile struct {
	Builds []struct {
		Build  int      `json:"build"`
		Assets []string `json:"assets"`
		Loads  []string `json:"loads"`
	} `json:"builds"`
}

type captured struct {
	file  string
	raw   []byte
	event *ingest.Event
	build int
}

func loadCaptures(t *testing.T) ([]captured, releasesFile) {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(testdata, "envelopes", "*.envelope"))
	if len(files) == 0 {
		t.Skip("no captured envelopes; run the spike harness (see README)")
	}
	var rel releasesFile
	b, err := os.ReadFile(filepath.Join(testdata, "releases.json"))
	if err != nil {
		t.Fatalf("releases.json: %v", err)
	}
	if err := json.Unmarshal(b, &rel); err != nil {
		t.Fatal(err)
	}
	buildOf := map[string]int{}
	for _, bl := range rel.Builds {
		for _, r := range bl.Loads {
			buildOf[r] = bl.Build
		}
	}
	var out []captured
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		env, err := ingest.ParseEnvelope(raw)
		if err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
			continue
		}
		evs, err := env.Events()
		if err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
		}
		if len(evs) == 0 {
			out = append(out, captured{file: f, raw: raw})
		}
		for _, ev := range evs {
			out = append(out, captured{file: f, raw: raw, event: ev, build: buildOf[ev.Release]})
		}
	}
	return out, rel
}

func toJS(ev *ingest.Event) normalize.JSError {
	ex := ev.MainException()
	e := normalize.JSError{Type: ex.Type, Message: ex.Value, PageURL: ev.PageURL()}
	if ex.Stacktrace != nil {
		for _, f := range ex.Stacktrace.Frames {
			fn := f.AbsPath
			if fn == "" {
				fn = f.Filename
			}
			e.Frames = append(e.Frames, normalize.Frame{Filename: fn, Function: f.Function, InApp: f.InApp})
		}
	}
	return e
}

func toNet(h *ingest.HTTPContext) normalize.NetFailure {
	return normalize.NetFailure{Method: h.Method, URL: h.URL, Status: h.Status, Body: h.Body, Error: h.Error}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func short(fp string) string {
	if len(fp) > 15 {
		return fp[:15]
	}
	return fp
}

var tags = normalize.Tags{Platform: "lovable", Stack: "supabase"}

// Gate 1: every captured envelope parses and every case produced an event in both builds.
func TestGateEnvelopesParse(t *testing.T) {
	caps, _ := loadCaptures(t)
	types := map[string]int{}
	seen := map[string]map[int]bool{}
	for _, c := range caps {
		if c.event == nil {
			continue
		}
		types["event"]++
		cs := c.event.Tags["spike_case"]
		if cs == "" {
			continue
		}
		if seen[cs] == nil {
			seen[cs] = map[int]bool{}
		}
		seen[cs][c.build] = true
	}
	files := map[string]bool{}
	for _, c := range caps {
		files[c.file] = true
	}
	t.Logf("%d envelope files, %d events", len(files), types["event"])
	for _, cs := range []string{"js_typeerror", "js_custom", "js_rejection", "net_22001", "net_42501", "net_auth", "net_404", "net_dead"} {
		if !seen[cs][1] || !seen[cs][2] {
			t.Errorf("case %s: events in build1=%v build2=%v", cs, seen[cs][1], seen[cs][2])
		}
	}
}

// Gate 2: JS fingerprints hold across redeploys (both frame rules reported).
func TestGateJSFingerprintStable(t *testing.T) {
	caps, _ := loadCaptures(t)
	rules := []normalize.FrameRule{normalize.RuleDropMinified, normalize.RuleFileOnly}
	// fps[rule][case][build] = set of fingerprints
	fps := map[normalize.FrameRule]map[string]map[int]map[string]bool{}
	for _, r := range rules {
		fps[r] = map[string]map[int]map[string]bool{}
	}
	for _, c := range caps {
		if c.event == nil || !strings.HasPrefix(c.event.Tags["spike_case"], "js_") || c.event.MainException() == nil {
			continue
		}
		cs := c.event.Tags["spike_case"]
		js := toJS(c.event)
		for _, r := range rules {
			if fps[r][cs] == nil {
				fps[r][cs] = map[int]map[string]bool{1: {}, 2: {}}
			}
			if c.build == 1 || c.build == 2 {
				fps[r][cs][c.build][normalize.JSFingerprint(js, r)] = true
			}
		}
		if c.build == 1 {
			var fr []string
			for _, f := range js.Frames {
				fr = append(fr, normalize.Filename(f.Filename)+":"+f.Function)
			}
			t.Logf("%s build1: %s | frames: %s", cs, normalize.JSSignature(js, tags), strings.Join(fr, " < "))
			t.Logf("   frames key drop-minified=%q file-only=%q", normalize.FramesKey(js, normalize.RuleDropMinified), normalize.FramesKey(js, normalize.RuleFileOnly))
		} else if c.build == 2 {
			var fr []string
			for _, f := range js.Frames {
				fr = append(fr, normalize.Filename(f.Filename)+":"+f.Function)
			}
			t.Logf("%s build2: frames: %s", cs, strings.Join(fr, " < "))
		}
	}
	for _, r := range rules {
		stable, distinct := true, map[string]string{}
		for cs, byBuild := range fps[r] {
			a, b := keys(byBuild[1]), keys(byBuild[2])
			same := len(a) == 1 && len(b) == 1 && a[0] == b[0]
			if !same {
				stable = false
			}
			t.Logf("rule %-13s %-13s build1=%v build2=%v stable=%v", r, cs, mapShort(a), mapShort(b), same)
			for _, fp := range append(a, b...) {
				if other, ok := distinct[fp]; ok && other != cs {
					t.Logf("rule %s: cases %s and %s share fingerprint", r, cs, other)
					stable = false
				}
				distinct[fp] = cs
			}
		}
		t.Logf("rule %s: stable and distinct = %v", r, stable)
		if r == normalize.DefaultFrameRule && !stable {
			t.Errorf("default rule %s is not stable across redeploys", r)
		}
	}
}

func mapShort(fps []string) []string {
	out := make([]string, len(fps))
	for i, f := range fps {
		out[i] = short(strings.TrimPrefix(f, "v1:"))
	}
	return out
}

// Gate 3: Supabase codes are extracted from captured bodies, and network
// fingerprints are stable across builds and distinct across cases.
func TestGateSupabaseBodies(t *testing.T) {
	caps, _ := loadCaptures(t)
	want := map[string]struct {
		backend, code string
		status        int
	}{
		"net_22001": {"postgrest", "22001", 400},
		"net_42501": {"postgrest", "42501", 0}, // 401 or 403 depending on role
		"net_auth":  {"supabase_auth", "", 400},
		"net_404":   {"postgrest", "", 404},
		"net_dead":  {"", "", 0},
	}
	fpByCase := map[string]map[string]bool{}
	for _, c := range caps {
		if c.event == nil {
			continue
		}
		cs := c.event.Tags["spike_case"]
		w, ok := want[cs]
		if !ok {
			continue
		}
		h, ok := c.event.HTTP()
		if !ok {
			continue // e.g. a JS-side error from the same click
		}
		n := toNet(h)
		res, _ := normalize.NetDetail(n)
		if c.build == 1 {
			t.Logf("%s: %s", cs, normalize.NetSignature(n, tags))
			t.Logf("   body: %s", h.Body)
		}
		if w.backend != "" && res.Backend != w.backend {
			t.Errorf("%s: backend %q, want %q (body %s)", cs, res.Backend, w.backend, h.Body)
		}
		if w.code != "" && res.Code != w.code {
			t.Errorf("%s: code %q, want %q", cs, res.Code, w.code)
		}
		if cs == "net_auth" && res.Code == "" {
			t.Errorf("net_auth: no error_code extracted from %s", h.Body)
		}
		if w.status != 0 && h.Status != w.status {
			t.Errorf("%s: status %d, want %d", cs, h.Status, w.status)
		}
		if cs == "net_dead" && h.Status != 0 {
			t.Errorf("net_dead: status %d, want 0", h.Status)
		}
		if fpByCase[cs] == nil {
			fpByCase[cs] = map[string]bool{}
		}
		fpByCase[cs][normalize.NetFingerprint(n)] = true
	}
	owner := map[string]string{}
	for cs := range want {
		fps := keys(fpByCase[cs])
		if len(fps) != 1 {
			t.Errorf("%s: %d distinct network fingerprints across builds, want 1", cs, len(fps))
		}
		for _, fp := range fps {
			if o, ok := owner[fp]; ok {
				t.Errorf("%s and %s share a network fingerprint", cs, o)
			}
			owner[fp] = cs
		}
	}
}

// Gate 4: release ID changes per build and is stable on reload; events carry it.
func TestGateReleaseHash(t *testing.T) {
	caps, rel := loadCaptures(t)
	if len(rel.Builds) != 2 {
		t.Fatalf("releases.json has %d builds, want 2", len(rel.Builds))
	}
	var ids []string
	for _, b := range rel.Builds {
		if len(b.Loads) < 2 || b.Loads[0] == "" || b.Loads[0] != b.Loads[1] {
			t.Errorf("build %d: release not stable on reload: %v", b.Build, b.Loads)
		}
		ids = append(ids, b.Loads[0])
		t.Logf("build %d: release %s assets %v", b.Build, b.Loads[0], b.Assets)
	}
	if ids[0] == ids[1] {
		t.Errorf("release did not change between builds: %s", ids[0])
	}
	for _, c := range caps {
		if c.event != nil && c.build == 0 {
			t.Errorf("%s: event release %q matches no build", filepath.Base(c.file), c.event.Release)
		}
	}
}

// Browser-side scrubbing: no emails or JWTs leave the page.
func TestGateScrubbed(t *testing.T) {
	caps, _ := loadCaptures(t)
	seen := map[string]bool{}
	for _, c := range caps {
		if seen[c.file] {
			continue
		}
		seen[c.file] = true
		s := string(c.raw)
		for _, bad := range []string{"@example.com", "eyJhbGci", "wrong-password"} {
			if strings.Contains(s, bad) {
				t.Errorf("%s contains %q", filepath.Base(c.file), bad)
			}
		}
	}
}

// Phase 3: JS fingerprint v2 is stable across builds and distinct per case.
func TestGateJSFingerprintV2Stable(t *testing.T) {
	caps, _ := loadCaptures(t)
	byCase := map[string]map[int]map[string]bool{}
	for _, c := range caps {
		cs := ""
		if c.event != nil {
			cs = c.event.Tags["spike_case"]
		}
		if !strings.HasPrefix(cs, "js_") || c.event.MainException() == nil {
			continue
		}
		js := toJS(c.event)
		if byCase[cs] == nil {
			byCase[cs] = map[int]map[string]bool{1: {}, 2: {}}
		}
		byCase[cs][c.build][normalize.JSFingerprintV2(js)] = true
		if c.build == 1 {
			t.Logf("%s: %s", cs, normalize.JSSignatureV2(js, tags))
		}
	}
	owner := map[string]string{}
	for cs, b := range byCase {
		a, z := keys(b[1]), keys(b[2])
		if len(a) != 1 || len(z) != 1 || a[0] != z[0] {
			t.Errorf("%s: v2 not stable: %v vs %v", cs, a, z)
			continue
		}
		if o, ok := owner[a[0]]; ok {
			t.Errorf("%s and %s share a v2 fingerprint", cs, o)
		}
		owner[a[0]] = cs
	}
}

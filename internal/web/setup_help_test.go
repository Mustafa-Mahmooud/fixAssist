package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/yourname/fixassist/internal/store/db"
)

func TestParseAppAddress(t *testing.T) {
	ok := map[string]string{
		"myapp.lovable.app":                         "myapp.lovable.app",
		"  https://MyApp.Lovable.app/login?x=1  ":   "myapp.lovable.app",
		"http://www.mybooking.com:8080/":            "mybooking.com",
		"localhost":                                 "localhost",
		"https://preview--myapp.lovable.app/#/home": "preview--myapp.lovable.app",
	}
	for in, want := range ok {
		if got, problem := parseAppAddress(in); got != want || problem != "" {
			t.Errorf("parseAppAddress(%q) = %q, %q; want %q", in, got, problem, want)
		}
	}
	bad := map[string]string{
		"":                                    "Paste your app's web address",
		"https://lovable.dev/projects/abc123": "Lovable's editor",
		"bolt.new/~/sb1-xyz":                  "Bolt's editor",
		"https://replit.com/@me/app":          "Replit's editor",
		"https://v0.dev/chat/abc":             "v0's editor",
		"myapp":                               "full web address",
		"my app.com":                          "doesn't look like a web address",
	}
	for in, want := range bad {
		if got, problem := parseAppAddress(in); got != "" || !strings.Contains(problem, want) {
			t.Errorf("parseAppAddress(%q) = %q, %q; want a problem with %q", in, got, problem, want)
		}
	}
}

func TestNameFromHost(t *testing.T) {
	for host, want := range map[string]string{
		"myapp.lovable.app": "myapp", "www.mybooking.com": "mybooking.com", "shop.vercel.app": "shop",
		"lovable.app": "lovable.app", "localhost": "localhost",
	} {
		if got := nameFromHost(host); got != want {
			t.Errorf("nameFromHost(%q) = %q, want %q", host, got, want)
		}
	}
}

const (
	testKey    = "0123456789abcdef0123456789abcdef"
	testIngest = "https://ingest.fixassist.test"
	ourTag     = `<script data-fix-sdk src="https://ingest.fixassist.test/sdk/` + testKey + `.js"></script>`
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fakeSite(status int, html string) *http.Client {
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(html)), Header: http.Header{}, Request: r}, nil
	})}
}

func TestCheckSite(t *testing.T) {
	p := db.GetOrgProjectRow{ID: 1, PublicKey: testKey, TargetAgent: "lovable", AllowedDomains: []string{"myapp.lovable.app"}}
	cases := []struct {
		name   string
		client *http.Client
		p      db.GetOrgProjectRow
		level  string
		title  string
	}{
		{"installed", fakeSite(200, "<head>"+ourTag+"</head>"), p, "good", "on your live site"},
		{"not published", fakeSite(404, "Not found"), p, "bad", "not found"},
		{"bot protection", fakeSite(403, "Just a moment..."), p, "warn", "didn't let us look"},
		{"no address", fakeSite(200, ""), db.GetOrgProjectRow{ID: 1}, "warn", "No address"},
		{"localhost", fakeSite(200, ""), db.GetOrgProjectRow{ID: 1, AllowedDomains: []string{"localhost"}}, "warn", "your own computer"},
	}
	for _, c := range cases {
		a := &App{IngestBase: testIngest, SiteClient: c.client}
		got := a.checkSite(context.Background(), c.p)
		if got.Level != c.level || !strings.Contains(got.Title, c.title) {
			t.Errorf("%s: got %s %q (%s)", c.name, got.Level, got.Title, got.Text)
		}
	}
}

func TestTestURL(t *testing.T) {
	if got := testURL([]string{"myapp.lovable.app", "other.com"}); got != "https://myapp.lovable.app/?fixassist-test" {
		t.Errorf("testURL = %q", got)
	}
	if testURL(nil) != "" || testURL([]string{"localhost"}) != "" {
		t.Error("no test link without a public address")
	}
}

// A default domain that forwards to the founder's own domain: offered with Allow.
func TestCheckSiteForwarded(t *testing.T) {
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "www.myshop.com" {
			return &http.Response{StatusCode: 301, Header: http.Header{"Location": {"https://www.myshop.com/"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("<head>" + ourTag + "</head>")), Request: r}, nil
	})}
	a := &App{IngestBase: testIngest, SiteClient: client}
	got := a.checkSite(context.Background(), db.GetOrgProjectRow{ID: 1, PublicKey: testKey, AllowedDomains: []string{"myshop.lovable.app"}})
	if got.Level != "bad" || got.AllowHost != "myshop.com" || !strings.Contains(got.Title, "forwards to www.myshop.com") {
		t.Errorf("forwarded: %+v", got)
	}
	// Once allowed, the same site passes.
	got = a.checkSite(context.Background(), db.GetOrgProjectRow{ID: 1, PublicKey: testKey, AllowedDomains: []string{"myshop.lovable.app", "myshop.com"}})
	if got.Level != "good" {
		t.Errorf("after allowing: %+v", got)
	}
}

func TestAgentDeeplink(t *testing.T) {
	const mcp, tok = "https://fix.example/mcp", "fa_secret"
	c := AgentDeeplink("cursor", mcp, tok)
	u, err := url.Parse(string(c.URL))
	if err != nil || u.Scheme != "cursor" || u.Query().Get("name") != "fix-assistant" {
		t.Fatalf("cursor link %q: %v", c.URL, err)
	}
	raw, err := base64.StdEncoding.DecodeString(u.Query().Get("config"))
	var cfg struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	if err != nil || json.Unmarshal(raw, &cfg) != nil || cfg.URL != mcp || cfg.Headers["Authorization"] != "Bearer "+tok {
		t.Errorf("cursor config %s: %v", raw, err)
	}
	v := AgentDeeplink("vscode", mcp, tok)
	link := strings.TrimPrefix(string(v.URL), "vscode:mcp/install?")
	if strings.Contains(link, "+") {
		t.Errorf("vscode link has a + (VS Code reads it literally, not as a space): %s", link)
	}
	q, _ := url.PathUnescape(link)
	if !strings.HasPrefix(string(v.URL), "vscode:mcp/install?") || !strings.Contains(q, `"type":"http"`) || !strings.Contains(q, `"name":"fix-assistant"`) || !strings.Contains(q, `"Authorization":"Bearer `+tok+`"`) {
		t.Errorf("vscode link %q", v.URL)
	}
	if AgentDeeplink("claude", mcp, tok) != nil || AgentDeeplink("", mcp, tok) != nil {
		t.Error("no deeplink for other tools")
	}
}

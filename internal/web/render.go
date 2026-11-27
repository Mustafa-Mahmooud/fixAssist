package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/yourname/fixassist/internal/auth"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

var reBacktick = regexp.MustCompile("`([^`]+)`")

// codeText escapes s and renders `names` as <code> (explanations use
// backticks for table, column and property names).
func codeText(s string) template.HTML {
	return template.HTML(reBacktick.ReplaceAllString(template.HTMLEscapeString(s), "<code>$1</code>"))
}

var funcs = template.FuncMap{
	"code":    codeText,
	"list":    func(s ...string) []string { return s },
	"inc":     func(i int) int { return i + 1 },
	"add":     func(a, b any) int64 { return toInt64(a) + toInt64(b) },
	"seconds": func(ms int64) string { return fmt.Sprintf("%.1f s", float64(ms)/1000) },
	"deviceName": func(d string) string {
		switch d {
		case "mobile":
			return "Phone"
		case "tablet":
			return "Tablet"
		case "desktop":
			return "Computer"
		}
		return "Unknown"
	},
	"insightOutcome": func(o string) string {
		switch o {
		case "worked":
			return "Helped"
		case "worse":
			return "Made it worse"
		}
		return "No change"
	},
	"outcomeLabel": func(o string) string {
		switch o {
		case "worked":
			return "Worked"
		case "failed":
			return "Didn't work"
		}
		return "Partly"
	},
	// pct is a/b as a whole percentage, for progress bars.
	"pct": func(a, b any) int64 {
		if d := toInt64(b); d > 0 {
			return min(100, toInt64(a)*100/d)
		}
		return 0
	},
	// statusHelp explains an issue status in plain words (badge tooltips).
	"statusHelp": func(s string) string {
		switch s {
		case "new":
			return "Just appeared. It will be explained automatically soon."
		case "explained":
			return "Explained, with a fix message ready to copy."
		case "verifying":
			return "You applied a fix; we're checking that the error stopped."
		case "fixed":
			return "Confirmed: it hasn't happened since the fix."
		case "open":
			return "Still happening, or it came back after a fix."
		case "ignored":
			return "You chose to ignore this one."
		case "analysis_failed":
			return "We couldn't explain it automatically. Try Explain now."
		}
		return ""
	},
	// initial is the first letter of an email, for the avatar in the header.
	"initial": func(s string) string {
		for _, r := range s {
			return strings.ToUpper(string(r))
		}
		return "?"
	},
	"agentName":   agentName,
	"tipFor":      tipFor,
	"backendName": backendName,
	"ago":         ago,
	"roleArgs":    func(m memberView, csrf string) map[string]any { return map[string]any{"M": m, "CSRF": csrf} },
	"prettyJSON":  prettyJSON,
	"kindName": func(k string) string {
		switch k {
		case "net":
			return "Network request"
		case "js":
			return "Page error"
		case "server":
			return "Server error"
		default:
			return "Message"
		}
	},
	"severityName": func(s string) string {
		switch s {
		case "high":
			return "High impact"
		case "medium":
			return "Medium impact"
		}
		return "Low impact"
	},
	"statusName": func(s string) string {
		switch s {
		case "new":
			return "New"
		case "explained":
			return "Explained"
		case "verifying":
			return "Checking fix"
		case "fixed":
			return "Fixed"
		case "open":
			return "Open"
		case "ignored":
			return "Ignored"
		case "analysis_failed":
			return "Not explained"
		}
		return s
	},
}

// pages are parsed once: layout + partials + one page each.
var pages = map[string]*template.Template{}

func init() {
	for _, p := range []string{"login", "magic", "projects", "setup", "issues", "issue", "alerts", "feedback", "admin", "admin_accounts", "admin_account", "admin_growth", "overview", "pages", "funnels", "insights", "admin_github", "welcome", "privacy"} {
		pages[p] = template.Must(template.New(p).Funcs(funcs).ParseFS(templateFS,
			"templates/layout.html", "templates/partials.html", "templates/"+p+".html"))
	}
	// Partials alone, for htmx responses.
	pages["partials"] = template.Must(template.New("partials").Funcs(funcs).ParseFS(templateFS, "templates/partials.html"))
}

// page is the data every template receives.
type page struct {
	Title   string
	Session *auth.Session
	Error   string
	Data    any
}

func (a *App) render(w http.ResponseWriter, status int, name string, p page) {
	a.withOnboarding(&p) // project pages: the "Getting started" checklist
	var buf bytes.Buffer
	if err := pages[name].ExecuteTemplate(&buf, "layout", p); err != nil {
		a.Log.Error("render", "page", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

func (a *App) partial(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := pages["partials"].ExecuteTemplate(&buf, name, data); err != nil {
		a.Log.Error("render partial", "name", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

func staticHandler() http.Handler {
	sub, _ := fs.Sub(staticFS, "static")
	files := http.FileServer(http.FS(sub))
	return http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		files.ServeHTTP(w, r)
	}))
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 48*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	case d < 60*24*time.Hour:
		return plural(int(d.Hours()/24), "day") + " ago"
	}
	return t.Format("2 Jan 2006")
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func prettyJSON(b []byte) string {
	var out bytes.Buffer
	if json.Indent(&out, b, "", "  ") != nil {
		return string(b)
	}
	return out.String()
}

// sparkline renders hourly counts as an inline SVG bar chart.
func sparkline(counts []int, label string) template.HTML {
	maxV := 0
	for _, c := range counts {
		maxV = max(maxV, c)
	}
	const h = 48.0
	w := float64(len(counts))
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="spark" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" role="img" aria-label="%s">`, w, h, template.HTMLEscapeString(label))
	for i, c := range counts {
		if c == 0 || maxV == 0 {
			continue
		}
		bh := math.Max(2, float64(c)/float64(maxV)*h)
		fmt.Fprintf(&b, `<rect x="%d" y="%.1f" width="0.8" height="%.1f"><title>%d</title></rect>`, i, h-bh, bh, c)
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// toInt64 lets template arithmetic mix sqlc's int32 and int64 columns.
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case int64:
		return n
	}
	return 0
}

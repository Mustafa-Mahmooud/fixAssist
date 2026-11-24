// Package ai explains issues and writes fix prompts.
//
// Phase 4 runs in "agent mode": the founder's own AI agent (Lovable, Bolt,
// Cursor, ...) does the code-level reasoning, since it can read the app's
// code and we can't. We supply what needs no LLM: what broke, who is
// affected, a catalog explanation for well-known errors, and an
// evidence-packed prompt that asks for exactly one change and says how to
// check it. The LLM and Embedder interfaces are kept for switching our own
// model on later (FDR "AI layer").
package ai

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/yourname/fixassist/internal/ingest"
	"github.com/yourname/fixassist/internal/normalize"
	"github.com/yourname/fixassist/internal/normalize/bodyparse"
)

// PromptVersion is stored with every suggestion (FDR "Versioning and evals").
const PromptVersion = "v1"

// NetSample is a failed request, as the founder's app saw it (scrubbed).
type NetSample struct {
	Method  string
	Path    string // URL template, no host: /rest/v1/profiles
	Host    string
	Status  int
	Backend string // postgrest, supabase_auth, firebase, json, raw, ""
	Code    string
	Message string
	Details string
	Hint    string
	Error   string // client-side error for status 0
	Body    string
}

// JSSample is a page error.
type JSSample struct {
	Type     string
	Message  string // scrubbed original
	Property string // identifier quoted by the engine, e.g. profile
	Function string // nearest non-minified function name
	Frames   []string
	// Set when response.json() failed (add-on contexts.fix_json).
	JSONPath   string
	JSONStatus int
	JSONType   string
}

// Context is everything we know about one issue, without customer secrets.
type Context struct {
	ProjectID    int64
	CodeRefs     []CodeRef // set when the project has a GitHub repo connected
	IssueID      int64
	Kind         string // js, net, message
	Title        string
	Signature    string
	Route        string
	Agent        string // lovable, bolt, cursor, replit, v0, other
	Backend      string // supabase, firebase, unknown
	KeyPage      bool
	Users        int64
	Events       int64
	Events24h    int64
	FirstSeen    time.Time
	FirstRelease string
	Regressions  int32
	Attempt      int // 2+ when the previous fix did not work
	Net          []NetSample
	JS           []JSSample
	Message      string
}

// MaxSamples is how many samples go into a prompt (FDR: up to 3).
const MaxSamples = 3

var reEngineIdent = regexp.MustCompile(`(?:\(reading '|\(evaluating '|can't access property "|')([A-Za-z_$][\w$.]*)['"]`)

// AddSample decodes one stored event payload into the context.
func (c *Context) AddSample(payload []byte) {
	if payload == nil || len(c.Net)+len(c.JS) >= MaxSamples {
		return
	}
	ev, err := ingest.ParseEvent(payload)
	if err != nil {
		return
	}
	if h, ok := ev.HTTP(); ok {
		n := ingest.NetFailure(h)
		var res bodyparse.Result
		if n.Body != "" {
			res = bodyparse.Parse(bodyparse.Input{Status: n.Status, URL: n.URL, Body: []byte(n.Body)})
		}
		s := NetSample{
			Method: strings.ToUpper(n.Method), Path: normalize.RouteTemplate(n.URL), Host: hostOf(n.URL),
			Status: n.Status, Backend: res.Backend, Code: res.Code, Message: res.Message,
			Error: n.Error, Body: n.Body,
		}
		s.Details, s.Hint = postgrestExtras(n.Body)
		c.Net = append(c.Net, s)
		return
	}
	if ex := ev.MainException(); ex != nil {
		js := ingest.JSError(ev)
		s := JSSample{Type: ex.Type, Message: ex.Value, Function: normalize.NearestNamedFrame(js)}
		if m := reEngineIdent.FindStringSubmatch(ex.Value); m != nil {
			s.Property = m[1]
		}
		if raw, ok := ev.Contexts["fix_json"]; ok {
			var fj struct {
				URL         string `json:"url"`
				Status      int    `json:"status"`
				ContentType string `json:"content_type"`
			}
			if json.Unmarshal(raw, &fj) == nil {
				s.JSONPath, s.JSONStatus, s.JSONType = normalize.RouteTemplate(fj.URL), fj.Status, fj.ContentType
			}
		}
		for i := len(js.Frames) - 1; i >= 0 && len(s.Frames) < 5; i-- {
			f := js.Frames[i]
			s.Frames = append(s.Frames, normalize.Filename(f.Filename)+" "+f.Function)
		}
		c.JS = append(c.JS, s)
		return
	}
	if c.Message == "" {
		c.Message = ev.MessageText()
	}
}

func hostOf(u string) string {
	o := normalize.Origin(u)
	return strings.TrimPrefix(strings.TrimPrefix(o, "https://"), "http://")
}

// TableFromPath returns the Supabase table in /rest/v1/<table>, or "".
func TableFromPath(path string) string {
	if t, ok := strings.CutPrefix(path, "/rest/v1/"); ok && t != "" && !strings.Contains(t, "/") {
		return t
	}
	return ""
}

// RPCFromPath returns the function in /rest/v1/rpc/<fn>, or "".
func RPCFromPath(path string) string {
	if f, ok := strings.CutPrefix(path, "/rest/v1/rpc/"); ok && !strings.Contains(f, "/") {
		return f
	}
	return ""
}

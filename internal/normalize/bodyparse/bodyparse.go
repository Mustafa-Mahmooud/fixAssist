// Package bodyparse extracts an error code and message from 4xx/5xx
// response bodies. A new backend means a new Parser, not a new pipeline.
package bodyparse

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Result is what a parser extracts from a response body.
type Result struct {
	Backend string // "postgrest", "supabase_auth", "firebase", "json", "raw", ""
	Code    string // e.g. "22001", "invalid_credentials"; "" if unknown
	Message string
}

// CodePrefix is the backend prefix used in signatures ("pg:22001").
func (r Result) CodePrefix() string {
	switch r.Backend {
	case "postgrest":
		return "pg"
	case "supabase_auth":
		return "auth"
	case "firebase":
		return "fb"
	default:
		return "code"
	}
}

// Input is a failed response. URL lets a parser use the endpoint as a hint
// when a body shape alone is ambiguous.
type Input struct {
	Status int
	URL    string
	Body   []byte
}

// Parser recognises one body shape.
type Parser interface {
	Name() string
	Parse(in Input) (Result, bool)
}

// Default is tried in order; Raw always matches a non-empty body.
var Default = []Parser{PostgREST{}, SupabaseAuth{}, Firebase{}, GenericJSON{}, Raw{}}

// Parse runs the Default parsers and returns the first match.
func Parse(in Input) Result {
	for _, p := range Default {
		if r, ok := p.Parse(in); ok {
			return r
		}
	}
	return Result{}
}

func object(body []byte) (map[string]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return nil, false
	}
	return m, true
}

func str(m map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := m[key]
	if !ok {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String(), true
	}
	return "", false
}

// PostgREST: {"code":"22001","details":null,"hint":null,"message":"value too long ..."}
type PostgREST struct{}

func (PostgREST) Name() string { return "postgrest" }

func (PostgREST) Parse(in Input) (Result, bool) {
	body := in.Body
	m, ok := object(body)
	if !ok {
		return Result{}, false
	}
	code, okc := str(m, "code")
	msg, okm := str(m, "message")
	_, hasDetails := m["details"]
	_, hasHint := m["hint"]
	if !okc || !okm || !(hasDetails || hasHint) {
		return Result{}, false
	}
	return Result{Backend: "postgrest", Code: code, Message: msg}, true
}

// SupabaseAuth (GoTrue) has three shapes seen in the wild:
//
//	{"code":400,"error_code":"invalid_credentials","msg":"..."}       (older self-hosted)
//	{"code":"invalid_credentials","message":"..."}                    (hosted, Nov 2026; Phase 1 spike)
//	{"error":"invalid_grant","error_description":"..."}               (OAuth form)
//
// The hosted shape is too generic to claim from the body alone, so it is
// only matched on /auth/v1/ endpoints.
type SupabaseAuth struct{}

func (SupabaseAuth) Name() string { return "supabase_auth" }

var reSnake = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func (SupabaseAuth) Parse(in Input) (Result, bool) {
	body := in.Body
	m, ok := object(body)
	if !ok {
		return Result{}, false
	}
	msg := firstStr(m, "msg", "message", "error_description")
	if code, ok := str(m, "error_code"); ok {
		return Result{Backend: "supabase_auth", Code: code, Message: msg}, true
	}
	if strings.Contains(in.URL, "/auth/v1/") {
		if code, ok := str(m, "code"); ok && reSnake.MatchString(code) {
			return Result{Backend: "supabase_auth", Code: code, Message: msg}, true
		}
	}
	if e, ok := str(m, "error"); ok {
		if _, ok := m["error_description"]; ok {
			return Result{Backend: "supabase_auth", Code: e, Message: msg}, true
		}
	}
	return Result{}, false
}

func firstStr(m map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		if s, ok := str(m, k); ok && s != "" {
			return s
		}
	}
	return ""
}

// Firebase: {"error":{"code":400,"message":"EMAIL_EXISTS","status":"INVALID_ARGUMENT"}}
type Firebase struct{}

func (Firebase) Name() string { return "firebase" }

var reUpperSnake = regexp.MustCompile(`^[A-Z][A-Z0-9_]+$`)

func (Firebase) Parse(in Input) (Result, bool) {
	body := in.Body
	m, ok := object(body)
	if !ok {
		return Result{}, false
	}
	inner, ok := object(m["error"])
	if !ok {
		return Result{}, false
	}
	msg, okm := str(inner, "message")
	st, oks := str(inner, "status")
	if !okm || !oks {
		return Result{}, false
	}
	code := st
	if c, _, _ := strings.Cut(msg, " "); reUpperSnake.MatchString(c) {
		code = c
	}
	return Result{Backend: "firebase", Code: code, Message: msg}, true
}

// GenericJSON looks for error / message / code keys.
type GenericJSON struct{}

func (GenericJSON) Name() string { return "json" }

func (GenericJSON) Parse(in Input) (Result, bool) {
	body := in.Body
	m, ok := object(body)
	if !ok {
		return Result{}, false
	}
	r := Result{Backend: "json"}
	r.Code, _ = str(m, "code")
	r.Message = firstStr(m, "message", "error", "detail", "msg")
	if r.Message == "" {
		if inner, ok := object(m["error"]); ok {
			r.Message = firstStr(inner, "message", "msg")
			if r.Code == "" {
				r.Code, _ = str(inner, "code")
			}
		}
	}
	if r.Code == "" && r.Message == "" {
		return Result{}, false
	}
	return r, true
}

// Raw uses the first 200 characters of an unparseable body.
type Raw struct{}

func (Raw) Name() string { return "raw" }

func (Raw) Parse(in Input) (Result, bool) {
	body := in.Body
	s := strings.TrimSpace(string(body))
	if s == "" {
		return Result{}, false
	}
	if utf8.RuneCountInString(s) > 200 {
		s = string([]rune(s)[:200])
	}
	return Result{Backend: "raw", Message: s}, true
}

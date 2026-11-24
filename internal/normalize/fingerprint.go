package normalize

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/yourname/fixassist/internal/normalize/bodyparse"
)

// Frame is a stack frame as normalize sees it.
type Frame struct {
	Filename string // raw filename or abs_path
	Function string
	InApp    *bool
}

// JSError is a JavaScript exception.
type JSError struct {
	Type    string
	Message string
	Frames  []Frame // oldest first, crash site last (Sentry order)
	PageURL string  // page the error happened on
}

// NetFailure is a failed fetch/XHR from the add-on.
type NetFailure struct {
	Method string
	URL    string
	Status int    // 0 for network failure (CORS, offline, timeout)
	Body   string // scrubbed, at most 2 KB
	Error  string // client-side error text for status 0
}

// FrameRule decides how frames contribute to a JS fingerprint.
type FrameRule int

const (
	// RuleDropMinified (FDR rule): use file + function of the top 3 in-app
	// frames, but drop all frames when any function name looks minified.
	RuleDropMinified FrameRule = iota
	// RuleFileOnly: always use the hash-stripped filenames of the top 3 in-app
	// frames and never function names.
	RuleFileOnly
)

func (r FrameRule) String() string {
	if r == RuleFileOnly {
		return "file-only"
	}
	return "drop-minified"
}

// DefaultFrameRule is the rule fingerprints use unless told otherwise.
// The Phase 1 spike compares both rules; see docs/spike-findings.md.
var DefaultFrameRule = RuleDropMinified

func hash(s string) string {
	return Version + ":" + hashHex(s)
}

func hashHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// inAppTop3 returns up to 3 crash-side frames from the page's own origin.
func inAppTop3(frames []Frame, pageURL string) []Frame {
	origin := Origin(pageURL)
	var out []Frame
	for i := len(frames) - 1; i >= 0 && len(out) < 3; i-- {
		f := frames[i]
		inApp := origin != "" && strings.HasPrefix(f.Filename, origin)
		if origin == "" && f.InApp != nil {
			inApp = *f.InApp
		}
		if inApp {
			out = append(out, f)
		}
	}
	return out
}

// shortName is the last dotted part of a function name ("Object.onClick" -> "onClick").
func shortName(fn string) string {
	if i := strings.LastIndexByte(fn, '.'); i >= 0 {
		return fn[i+1:]
	}
	return fn
}

// LooksMinified reports whether a function name is a minifier output
// (1-2 characters) or unknown.
func LooksMinified(fn string) bool {
	n := shortName(fn)
	return n == "" || n == "?" || len(n) <= 2
}

// FramesKey is the frame part of the JS fingerprint under the given rule.
func FramesKey(e JSError, rule FrameRule) string {
	top := inAppTop3(e.Frames, e.PageURL)
	parts := make([]string, 0, len(top))
	for _, f := range top {
		if rule == RuleDropMinified && LooksMinified(f.Function) {
			return ""
		}
		file := Filename(f.Filename)
		if rule == RuleFileOnly {
			parts = append(parts, file)
		} else {
			parts = append(parts, file+":"+shortName(f.Function))
		}
	}
	return strings.Join(parts, ",")
}

// JSFingerprint hashes type + message + top-3 in-app frames + route.
func JSFingerprint(e JSError, rule FrameRule) string {
	return hash(Version + "|js|" + e.Type + "|" + Message(e.Message) + "|" + FramesKey(e, rule) + "|" + RouteTemplate(e.PageURL))
}

// MessageFingerprint is for events with no exception (captureMessage).
func MessageFingerprint(message, pageURL string) string {
	return hash(Version + "|msg|" + Message(message) + "|" + RouteTemplate(pageURL))
}

// NetDetail is the parsed body plus the normalized error message.
func NetDetail(n NetFailure) (bodyparse.Result, string) {
	res := bodyparse.Result{}
	if n.Body != "" {
		res = bodyparse.Parse(bodyparse.Input{Status: n.Status, URL: n.URL, Body: []byte(n.Body)})
	}
	msg := res.Message
	if msg == "" {
		msg = n.Error
	}
	return res, Message(msg)
}

// NetFingerprint hashes method + URL template + status + error code + message.
func NetFingerprint(n NetFailure) string {
	res, msg := NetDetail(n)
	return hash(Version + "|net|" + strings.ToUpper(n.Method) + "|" + URLTemplate(n.URL) + "|" +
		strconv.Itoa(n.Status) + "|" + res.Code + "|" + msg)
}

// Tags are the project's platform/stack labels added to signatures.
type Tags struct {
	Platform string // lovable, bolt, cursor, replit, v0, other
	Stack    string // supabase, firebase, unknown
}

func (t Tags) suffix() string {
	var b strings.Builder
	if t.Platform != "" {
		b.WriteString(" platform=" + t.Platform)
	}
	if t.Stack != "" {
		b.WriteString(" stack=" + t.Stack)
	}
	return b.String()
}

// JSSignature is readable normalized text for embedding.
func JSSignature(e JSError, t Tags) string {
	return "js " + e.Type + " \"" + Message(e.Message) + "\" route=" + RouteTemplate(e.PageURL) + t.suffix()
}

// NetSignature, e.g.
// net POST /rest/v1/users 400 pg:22001 "value too long ..." platform=lovable stack=supabase
func NetSignature(n NetFailure, t Tags) string {
	res, msg := NetDetail(n)
	code := ""
	if res.Code != "" {
		code = " " + res.CodePrefix() + ":" + res.Code
	}
	return "net " + strings.ToUpper(n.Method) + " " + URLTemplate(n.URL) + " " + strconv.Itoa(n.Status) +
		code + " \"" + msg + "\"" + t.suffix()
}

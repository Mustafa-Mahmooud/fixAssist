// Package normalize turns raw error text, URLs and filenames into stable,
// PII-free forms used for fingerprints and signatures.
package normalize

import (
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Version prefixes every fingerprint and signature so rule changes never
// silently merge old and new issues.
const Version = "v1"

const maxMessage = 500

var (
	reJWT    = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*`)
	reUUID   = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	reEmail  = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	reQuoted = regexp.MustCompile("\"[^\"\\n]*\"|'[^'\\n]*'|`[^`\\n]*`")
	reHexy   = regexp.MustCompile(`(?i)\b(?:0x)?[0-9a-f]{12,}\b`)
	reNum    = regexp.MustCompile(`\b\d{3,}\b`)
	reSpace  = regexp.MustCompile(`\s+`)
	reDigits = regexp.MustCompile(`^\d+$`)
)

// scrubTokens are the placeholders the SDK add-on's scrubber writes.
var scrubTokens = strings.NewReplacer("[card]", "<n>", "[email]", "<email>", "[jwt]", "<jwt>", "[token]", "<token>", "[secret]", "<secret>")

// Message replaces volatile values with placeholders:
// JWTs, UUIDs, emails, quoted literals, long hex and numbers of 3+ digits.
func Message(s string) string {
	// Map browser-scrub placeholders onto the same tokens, so a value the
	// browser happened to scrub (e.g. a Luhn-valid order number) fingerprints
	// the same as one it didn't.
	s = scrubTokens.Replace(s)
	s = reJWT.ReplaceAllString(s, "<jwt>")
	s = reUUID.ReplaceAllString(s, "<uuid>")
	s = reEmail.ReplaceAllString(s, "<email>")
	s = reQuoted.ReplaceAllString(s, "<str>")
	s = reHexy.ReplaceAllStringFunc(s, func(m string) string {
		if reDigits.MatchString(m) {
			return "<n>"
		}
		return "<hex>"
	})
	s = reNum.ReplaceAllString(s, "<n>")
	s = strings.TrimSpace(reSpace.ReplaceAllString(s, " "))
	if len(s) > maxMessage {
		s = s[:maxMessage]
	}
	return s
}

// isIDLike reports whether a URL path segment is an identifier rather than
// part of the route.
func isIDLike(seg string) bool {
	if seg == "" {
		return false
	}
	if reDigits.MatchString(seg) || reUUID.MatchString(seg) || reEmail.MatchString(seg) {
		return true
	}
	hasDigit, hasLetter := false, false
	for _, r := range seg {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			hasLetter = true
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	// Mixed letters + digits, long enough to be a key/slug-id (e.g. hex ids, nanoids).
	return hasDigit && hasLetter && len(seg) >= 8
}

// URLTemplate returns the host-relative path with ID-like segments replaced
// and query values dropped, keeping sorted query keys:
//
//	https://x.supabase.co/rest/v1/users?select=*&id=eq.5  ->  /rest/v1/users?id&select
func URLTemplate(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return Message(raw)
	}
	segs := strings.Split(u.Path, "/")
	for i, s := range segs {
		if isIDLike(s) {
			segs[i] = "<id>"
		}
	}
	p := strings.Join(segs, "/")
	if p == "" {
		p = "/"
	}
	keys := queryKeys(u.RawQuery)
	if len(keys) > 0 {
		p += "?" + strings.Join(keys, "&")
	}
	return p
}

// RouteTemplate is URLTemplate without the query: the page route.
func RouteTemplate(raw string) string {
	t := URLTemplate(raw)
	if i := strings.IndexByte(t, '?'); i >= 0 {
		t = t[:i]
	}
	return t
}

func queryKeys(rawQuery string) []string {
	if rawQuery == "" {
		return nil
	}
	seen := map[string]bool{}
	for _, part := range strings.Split(rawQuery, "&") {
		k, _, _ := strings.Cut(part, "=")
		if k, err := url.QueryUnescape(k); err == nil && k != "" {
			seen[k] = true
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var (
	// Vite/Rollup: name-<8 base64url chars>.js
	reHash8 = regexp.MustCompile(`^(.+)[-.]([A-Za-z0-9_-]{8})$`)
	// webpack and others: name.<8-32 hex>.js
	reHashHex = regexp.MustCompile(`^(.+)[-.]([0-9a-f]{8,32})$`)
	reLineCol = regexp.MustCompile(`(:\d+)+$`)
)

func looksLikeHash(h string) bool {
	hasDigit, hasUpper, hasLower := false, false, false
	for _, r := range h {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= 'a' && r <= 'z':
			hasLower = true
		}
	}
	return hasDigit || (hasUpper && hasLower)
}

// Filename returns the host-relative path of a frame's file with bundle
// hashes, query strings and line/column numbers removed:
//
//	https://app.lovable.app/assets/index-BwXk3Q9a.js:12:345  ->  /assets/index.js
func Filename(raw string) string {
	s := reLineCol.ReplaceAllString(strings.TrimSpace(raw), "")
	if u, err := url.Parse(s); err == nil && u.Scheme != "" {
		s = u.Path
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	dir, base := path.Split(s)
	ext := path.Ext(base)
	name := strings.TrimSuffix(base, ext)
	if m := reHashHex.FindStringSubmatch(name); m != nil {
		name = m[1]
	} else if m := reHash8.FindStringSubmatch(name); m != nil && looksLikeHash(m[2]) {
		name = m[1]
	}
	return dir + name + ext
}

// Origin returns scheme://host of a URL, or "".
func Origin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

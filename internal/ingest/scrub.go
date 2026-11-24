package ingest

import (
	"net"
	"regexp"
	"strings"
)

// Server-side scrubbing repeats the browser rules (pkg/sdk-addon/addon.js),
// so data from an old or modified SDK is still cleaned. Keep the two in sync;
// scrub_test.go shares its fixtures with the add-on's rules.

var (
	reScrubEmail  = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	reScrubJWT    = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*`)
	reScrubBearer = regexp.MustCompile(`(?i)\b(Bearer|Basic)\s+[A-Za-z0-9._~+/=-]+`)
	reScrubCard   = regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`)
	// Whole key names only (so "weak_password" or "token_type" aren't hit),
	// and never object/array values, so JSON bodies stay parseable.
	reScrubKV    = regexp.MustCompile(`(?i)(^|[^\w])("?(?:password|passwd|secret|token|api[_-]?key|apikey|access_token|refresh_token)"?\s*[:=]\s*)("[^"]*"|[^\s,&}\]{\["][^\s,&}\]]*)`)
	reScrubQuery = regexp.MustCompile(`([?&][^=&#\s"]+)=[^&#\s"]*`)
)

func luhn(digits string) bool {
	sum, alt := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return sum%10 == 0
}

// ScrubString removes emails, JWTs, bearer tokens, secrets in key=value form,
// Luhn-valid card numbers and URL query values.
func ScrubString(s string) string {
	s = reScrubJWT.ReplaceAllString(s, "[jwt]")
	s = reScrubBearer.ReplaceAllString(s, "$1 [token]")
	s = reScrubKV.ReplaceAllStringFunc(s, func(m string) string {
		sub := reScrubKV.FindStringSubmatch(m)
		if strings.HasPrefix(sub[3], `"`) {
			return sub[1] + sub[2] + `"[secret]"`
		}
		return sub[1] + sub[2] + "[secret]"
	})
	s = reScrubEmail.ReplaceAllString(s, "[email]")
	s = reScrubCard.ReplaceAllStringFunc(s, func(m string) string {
		if luhn(strings.NewReplacer(" ", "", "-", "").Replace(m)) {
			return "[card]"
		}
		return m
	})
	return reScrubQuery.ReplaceAllString(s, "$1=")
}

func scrubValue(v any, depth int) any {
	if depth > 16 {
		return nil
	}
	switch t := v.(type) {
	case string:
		return ScrubString(t)
	case []any:
		for i := range t {
			t[i] = scrubValue(t[i], depth+1)
		}
		return t
	case map[string]any:
		for k, x := range t {
			t[k] = scrubValue(x, depth+1)
		}
		return t
	default:
		return v
	}
}

var sensitiveHeader = regexp.MustCompile(`(?i)^(authorization|cookie|set-cookie|apikey|x-api-key|proxy-authorization)$`)

// TruncateIP keeps the /24 (IPv4) or /48 (IPv6) network.
func TruncateIP(s string) string {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil {
		return ""
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.Mask(net.CIDRMask(24, 32)).String()
	}
	return ip.Mask(net.CIDRMask(48, 128)).String()
}

// ScrubEvent cleans a decoded event in place.
func ScrubEvent(ev map[string]any) {
	if req, ok := ev["request"].(map[string]any); ok {
		delete(req, "cookies")
		delete(req, "data")
		if h, ok := req["headers"].(map[string]any); ok {
			for k := range h {
				if sensitiveHeader.MatchString(k) {
					delete(h, k)
				}
			}
		}
	}
	if u, ok := ev["user"].(map[string]any); ok {
		delete(u, "email")
		delete(u, "username")
		if ip, ok := u["ip_address"].(string); ok {
			if t := TruncateIP(ip); t != "" {
				u["ip_address"] = t
			} else {
				delete(u, "ip_address")
			}
		}
	}
	// Typed input never leaves; breadcrumbs may be an array or {"values": [...]}.
	filter := func(list []any) []any {
		out := list[:0]
		for _, b := range list {
			if m, ok := b.(map[string]any); ok && m["category"] == "ui.input" {
				continue
			}
			out = append(out, b)
		}
		return out
	}
	switch b := ev["breadcrumbs"].(type) {
	case []any:
		ev["breadcrumbs"] = filter(b)
	case map[string]any:
		if vals, ok := b["values"].([]any); ok {
			b["values"] = filter(vals)
		}
	}
	scrubValue(ev, 0)
}

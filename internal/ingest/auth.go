package ingest

import (
	"net/http"
	"net/url"
	"strings"
)

// PublicKey extracts the DSN public key from, in order:
// the X-Sentry-Auth header, the sentry_key query parameter (what the browser
// SDK uses), or the dsn in the envelope header.
func PublicKey(r *http.Request, envDSN string) string {
	if h := r.Header.Get("X-Sentry-Auth"); h != "" {
		if k := parseAuthHeader(h)["sentry_key"]; k != "" {
			return k
		}
	}
	if k := r.URL.Query().Get("sentry_key"); k != "" {
		return k
	}
	if envDSN != "" {
		if u, err := url.Parse(envDSN); err == nil && u.User != nil {
			return u.User.Username()
		}
	}
	return ""
}

// parseAuthHeader parses `Sentry sentry_key=abc, sentry_version=7, ...`.
func parseAuthHeader(h string) map[string]string {
	out := map[string]string{}
	h = strings.TrimSpace(h)
	if len(h) >= 7 && strings.EqualFold(h[:7], "sentry ") {
		h = h[7:]
	}
	for _, part := range strings.Split(h, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// Package analytics is Phase 6 usage analytics: visitor identity, country,
// rollups for the dashboard, goals, funnels and weekly growth insights.
package analytics

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/oschwald/maxminddb-golang/v2"
)

// Enricher derives visitor identity and country at ingest. Raw IPs and user
// agents are used only here and never stored.
type Enricher struct {
	Secret     []byte // APP_SECRET: base of the daily visitor salt
	TrustProxy bool   // take the client IP from X-Forwarded-For (behind Caddy)
	geo        *maxminddb.Reader
}

// OpenGeo loads a DB-IP Lite / MaxMind country .mmdb file. An empty path
// means no country lookups.
func (e *Enricher) OpenGeo(path string) error {
	if path == "" {
		return nil
	}
	r, err := maxminddb.Open(path)
	if err != nil {
		return err
	}
	e.geo = r
	return nil
}

// ClientIP is the caller's IP: the first X-Forwarded-For entry when behind
// our proxy, else the socket address.
func (e *Enricher) ClientIP(r *http.Request) string {
	if e.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			return strings.TrimSpace(first)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Country is the ISO country code for ip, or "".
func (e *Enricher) Country(ip string) string {
	if e == nil || e.geo == nil {
		return ""
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	var rec struct {
		Country struct {
			ISOCode string `maxminddb:"iso_code"`
		} `maxminddb:"country"`
	}
	if err := e.geo.Lookup(addr).Decode(&rec); err != nil {
		return ""
	}
	return rec.Country.ISOCode
}

func hmacHex(key []byte, parts ...string) string {
	m := hmac.New(sha256.New, key)
	for _, p := range parts {
		m.Write([]byte(p))
		m.Write([]byte{0})
	}
	return hex.EncodeToString(m.Sum(nil))[:32]
}

// Visitor returns the anonymous visitor hash (FDR: cookieless by default).
//   - With consent the SDK sends a first-party id (vid): HMAC(project secret, vid).
//   - Otherwise: HMAC(daily salt, ip, user agent, project). The salt is
//     derived from APP_SECRET and the UTC date, so the same person gets a new
//     hash every day and can't be followed across days.
func (e *Enricher) Visitor(projectSecret []byte, projectID int64, vid, ip, userAgent string, now time.Time) string {
	if vid != "" {
		return "v:" + hmacHex(projectSecret, "vid", vid)
	}
	salt := hmacHex(e.Secret, "visitor-salt", now.UTC().Format("2006-01-02"))
	return "d:" + hmacHex([]byte(salt), ip, userAgent, strconv.FormatInt(projectID, 10))
}

// Device normalizes the SDK's viewport class.
func Device(d string) string {
	switch d {
	case "mobile", "tablet", "desktop":
		return d
	}
	return "unknown"
}

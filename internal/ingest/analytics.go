package ingest

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yourname/fixassist/internal/analytics"
	"github.com/yourname/fixassist/internal/normalize"
)

// AnalyticsRow is one usage event ready to store (FDR Phase 6).
type AnalyticsRow struct {
	TS           time.Time
	Type         string // session_start, click, custom, identify, signup, login
	SessionID    string
	VisitorHash  string
	AccountHash  string
	Route        string
	Name         string // click label or custom event name
	Props        []byte // JSON object or nil
	ReferrerHost string
	UTMSource    string
	UTMMedium    string
	UTMCampaign  string
	Country      string
	Device       string
}

type analyticsBatch struct {
	SessionID string        `json:"session_id"`
	VID       string        `json:"vid"`
	Device    string        `json:"device"`
	Events    []analyticsIn `json:"events"`
}

type analyticsIn struct {
	Type     string            `json:"type"`
	Path     string            `json:"path"`
	Name     string            `json:"name"`
	Props    map[string]any    `json:"props"`
	Referrer string            `json:"referrer"`
	UTM      map[string]string `json:"utm"`
	Account  string            `json:"account"`
}

// maxAnalyticsPerRequest bounds one batch (the SDK flushes every few seconds).
const maxAnalyticsPerRequest = 50

var analyticsTypes = map[string]bool{"session_start": true, "click": true, "custom": true, "identify": true, "signup": true, "login": true, "vital": true}

// validVital checks a page-speed measurement: a known metric and a value in
// a sane range (props.v). Anything else is dropped, so a broken or hostile
// client can't skew the percentiles with impossible numbers.
func validVital(name string, props map[string]any) bool {
	v, ok := props["v"].(float64)
	if !ok || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return false
	}
	switch name {
	case "CLS":
		return v <= 10
	case "LCP", "FCP", "TTFB", "INP":
		return v <= 120_000
	}
	return false
}

// cleanProps keeps up to 10 simple values, scrubbed and truncated. Objects
// and arrays are dropped: props are for counting, not for storing records.
func cleanProps(in map[string]any) []byte {
	if len(in) == 0 {
		return nil
	}
	out := map[string]any{}
	// A measurement's value always survives the 10-key cap (map order is
	// random, and a vital carries up to 10 props).
	if v, ok := in["v"].(float64); ok {
		out["v"] = v
	}
	for k, v := range in {
		if len(out) >= 10 || len(k) > 40 {
			continue
		}
		switch t := v.(type) {
		case string:
			out[k] = truncate(ScrubString(t), 100)
		case float64, bool:
			out[k] = t
		}
	}
	if len(out) == 0 {
		return nil
	}
	b, _ := json.Marshal(out)
	return b
}

func referrerHost(ref, ownOrigin string) string {
	u, err := url.Parse(ref)
	if err != nil || u.Host == "" {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if o, err := url.Parse(ownOrigin); err == nil && strings.TrimPrefix(strings.ToLower(o.Hostname()), "www.") == host {
		return "" // internal navigation isn't a source
	}
	return host
}

// parseAnalytics validates a batch and enriches it with the visitor hash,
// country and device. Raw IPs, user agents and account ids are not kept.
// backend is what the add-on detected from the app's requests ("stack"
// event: supabase or firebase), or "".
func (h *Handler) parseAnalytics(data []byte, r *http.Request, projectSecret []byte, projectID int64, now time.Time) (rows []AnalyticsRow, backend string, err error) {
	var b analyticsBatch
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, "", err
	}
	if len(b.Events) > maxAnalyticsPerRequest {
		b.Events = b.Events[:maxAnalyticsPerRequest]
	}
	visitor, country := "", ""
	if h.Enrich != nil {
		ip := h.Enrich.ClientIP(r)
		visitor = h.Enrich.Visitor(projectSecret, projectID, truncate(b.VID, 64), ip, r.UserAgent(), now)
		country = h.Enrich.Country(ip)
	}
	rows = make([]AnalyticsRow, 0, len(b.Events))
	for _, e := range b.Events {
		if e.Type == "stack" {
			backend = e.Name
			continue
		}
		if !analyticsTypes[e.Type] {
			continue
		}
		row := AnalyticsRow{
			TS: now, Type: e.Type, SessionID: truncate(b.SessionID, 64), VisitorHash: visitor,
			Route: truncate(normalize.RouteTemplate(e.Path), 500), Name: truncate(ScrubString(strings.TrimSpace(e.Name)), 100),
			Props: cleanProps(e.Props), Country: country, Device: analytics.Device(b.Device),
		}
		switch e.Type {
		case "session_start":
			row.ReferrerHost = truncate(referrerHost(e.Referrer, r.Header.Get("Origin")), 200)
			row.UTMSource = truncate(strings.ToLower(e.UTM["utm_source"]), 100)
			row.UTMMedium = truncate(strings.ToLower(e.UTM["utm_medium"]), 100)
			row.UTMCampaign = truncate(strings.ToLower(e.UTM["utm_campaign"]), 100)
		case "identify":
			if e.Account == "" {
				continue
			}
			row.AccountHash = UserHash(projectSecret, e.Account)
		case "custom":
			if row.Name == "" {
				continue
			}
		case "vital":
			if !validVital(e.Name, e.Props) {
				continue
			}
		}
		rows = append(rows, row)
	}
	return rows, backend, nil
}

func (h *Handler) analyticsEvents(w http.ResponseWriter, r *http.Request) {
	cors(w)
	data, err := readBody(w, r)
	if err != nil {
		h.fail(w, statusFor(err), "bad body", err)
		return
	}
	p, ok := h.resolve(w, r, PublicKey(r, ""), "", false)
	if !ok {
		return
	}
	rows, backend, err := h.parseAnalytics(data, r, p.HMACSecret, p.ID, h.now())
	if err != nil {
		h.fail(w, http.StatusBadRequest, "bad json", nil)
		return
	}
	h.noteBackend(r.Context(), p, backend)
	if len(rows) > 0 {
		rec := Received{Project: p, PublicKey: p.PublicKey, Raw: data, Analytics: rows, Origin: r.Header.Get("Origin")}
		if err := h.Sink.Analytics(r.Context(), rec); err != nil {
			h.fail(w, http.StatusServiceUnavailable, "unavailable", fmt.Errorf("analytics: %w", err))
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

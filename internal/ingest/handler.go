package ingest

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yourname/fixassist/internal/analytics"
	"github.com/yourname/fixassist/internal/normalize"
	"github.com/yourname/fixassist/internal/projects"
)

// Received is what a Sink gets for each accepted envelope or page-view batch.
type Received struct {
	Project   projects.Project
	ProjectID string // from the URL path
	PublicKey string
	Raw       []byte    // decompressed body
	Envelope  *Envelope // nil for page views
	Events    []*Event
	Rows      []EventRow
	Pageviews []PageviewRow
	Analytics []AnalyticsRow
	Origin    string
}

// Sink stores accepted payloads: Postgres (PGSink) or files (FileSink).
type Sink interface {
	Envelope(ctx context.Context, rec Received) error
	Pageview(ctx context.Context, rec Received) error
	Analytics(ctx context.Context, rec Received) error
}

// ProjectLookup resolves a DSN public key.
type ProjectLookup interface {
	Lookup(ctx context.Context, publicKey string) (projects.Project, bool, error)
}

// Handler serves the public ingest endpoints. Projects, Limits and Samples
// may be nil (the spike capture mode accepts any key, unlimited).
type Handler struct {
	Sink     Sink
	Log      *slog.Logger
	Projects ProjectLookup
	Limits   *projects.Limits
	Samples  *SampleCap
	Enrich   *analytics.Enricher // visitor hash + country for usage analytics (optional)
	Now      func() time.Time
	// Wrap, if set, instruments each endpoint (obs.Metrics.Wrap).
	Wrap func(endpoint string, h http.HandlerFunc) http.HandlerFunc
}

// Register mounts the ingest routes on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	wrap := h.Wrap
	if wrap == nil {
		wrap = func(_ string, f http.HandlerFunc) http.HandlerFunc { return f }
	}
	mux.HandleFunc("POST /api/{project_id}/envelope/", wrap("ingest.envelope", h.envelope))
	mux.HandleFunc("POST /api/{project_id}/store/", wrap("ingest.store", h.store))
	mux.HandleFunc("POST /v1/pv", wrap("ingest.pageview", h.pageview))
	mux.HandleFunc("POST /v1/a", wrap("ingest.analytics", h.analyticsEvents))
	for _, p := range []string{"/api/{project_id}/envelope/", "/api/{project_id}/store/", "/v1/pv", "/v1/a"} {
		mux.HandleFunc("OPTIONS "+p, preflight)
	}
}

// now is truncated to microseconds: Postgres timestamptz precision, so the
// (id, received_at) pair in job args matches the stored row exactly.
func (h *Handler) now() time.Time {
	t := time.Now()
	if h.Now != nil {
		t = h.Now()
	}
	return t.UTC().Truncate(time.Microsecond)
}

func cors(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Expose-Headers", "Retry-After, X-Sentry-Rate-Limits")
}

func preflight(w http.ResponseWriter, _ *http.Request) {
	cors(w)
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Encoding, X-Sentry-Auth")
	w.Header().Set("Access-Control-Max-Age", "86400")
	w.WriteHeader(http.StatusNoContent)
}

// readBody applies the 1 MB cap before and after decompression.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	body := http.MaxBytesReader(w, r.Body, MaxEnvelopeBytes)
	var rd io.Reader = body
	switch strings.ToLower(r.Header.Get("Content-Encoding")) {
	case "gzip":
		gz, err := gzip.NewReader(body)
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		defer gz.Close()
		rd = gz
	case "deflate":
		// HTTP "deflate" is usually zlib-wrapped; fall back to raw deflate.
		compressed, err := io.ReadAll(body)
		if err != nil {
			return nil, err
		}
		if zr, zerr := zlib.NewReader(bytes.NewReader(compressed)); zerr == nil {
			defer zr.Close()
			rd = zr
		} else {
			rd = flate.NewReader(bytes.NewReader(compressed))
		}
	}
	data, err := io.ReadAll(io.LimitReader(rd, MaxEnvelopeBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxEnvelopeBytes {
		return nil, ErrTooLarge
	}
	return data, nil
}

func (h *Handler) fail(w http.ResponseWriter, status int, msg string, err error) {
	if err != nil {
		h.Log.Warn("ingest rejected", "status", status, "msg", msg, "err", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"detail":` + jsonString(msg) + `}`))
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func statusFor(err error) int {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) || errors.Is(err, ErrTooLarge) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

// resolve authenticates the key and checks the project id and origin.
// It writes the error response and returns ok=false on failure.
func (h *Handler) resolve(w http.ResponseWriter, r *http.Request, key, pathProject string, allowNoOrigin bool) (projects.Project, bool) {
	if key == "" {
		h.fail(w, http.StatusUnauthorized, "missing sentry_key", nil)
		return projects.Project{}, false
	}
	if h.Projects == nil {
		return projects.Project{PublicKey: key}, true
	}
	p, ok, err := h.Projects.Lookup(r.Context(), key)
	if err != nil {
		h.fail(w, http.StatusServiceUnavailable, "unavailable", err)
		return p, false
	}
	if !ok {
		h.fail(w, http.StatusUnauthorized, "unknown or revoked key", nil)
		return p, false
	}
	if pathProject != "" && pathProject != strconv.FormatInt(p.ID, 10) {
		h.fail(w, http.StatusForbidden, "key does not belong to this project", nil)
		return p, false
	}
	origin := r.Header.Get("Origin")
	// Server SDKs send no Origin. Browsers always send one on these
	// cross-origin POSTs, so allowing its absence doesn't open the endpoint
	// to other websites; the domain list still applies to every browser.
	if !(origin == "" && allowNoOrigin) && !projects.OriginAllowed(p.AllowedDomains, origin) {
		h.fail(w, http.StatusForbidden, "origin not allowed for this project", nil)
		return p, false
	}
	return p, true
}

// limit applies the rate and monthly quota; writes 429 when over.
func (h *Handler) limit(w http.ResponseWriter, p projects.Project, n int) bool {
	if h.Limits == nil {
		return true
	}
	d := h.Limits.Allow(p, n)
	if d.Allowed {
		return true
	}
	secs := int64(d.RetryAfter / time.Second)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
	// Sentry SDK format: <seconds>:<categories, empty = all>:<scope>
	w.Header().Set("X-Sentry-Rate-Limits", fmt.Sprintf("%d::key", secs))
	h.fail(w, http.StatusTooManyRequests, "over "+d.Reason+" limit", nil)
	return false
}

func (h *Handler) envelope(w http.ResponseWriter, r *http.Request) {
	cors(w)
	data, err := readBody(w, r)
	if err != nil {
		h.fail(w, statusFor(err), "bad body", err)
		return
	}
	env, err := ParseEnvelope(data)
	if err != nil {
		h.fail(w, http.StatusBadRequest, "bad envelope", err)
		return
	}
	h.accept(w, r, data, env)
}

func (h *Handler) store(w http.ResponseWriter, r *http.Request) {
	cors(w)
	data, err := readBody(w, r)
	if err != nil {
		h.fail(w, statusFor(err), "bad body", err)
		return
	}
	// Legacy store: the body is a single event. Wrap it as an envelope.
	env := &Envelope{Items: []Item{{Header: ItemHeader{Type: ItemEvent}, Payload: data}}}
	h.accept(w, r, data, env)
}

func (h *Handler) accept(w http.ResponseWriter, r *http.Request, data []byte, env *Envelope) {
	p, ok := h.resolve(w, r, PublicKey(r, env.Header.DSN), r.PathValue("project_id"), true)
	if !ok {
		return
	}
	events, err := env.Events()
	if err != nil {
		h.fail(w, http.StatusBadRequest, "bad event", err)
		return
	}
	ack := func() {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":` + jsonString(env.Header.EventID) + `}`))
	}
	if len(events) == 0 {
		ack() // sessions, client reports, transactions, replays: ack and drop
		return
	}
	if !h.limit(w, p, len(events)) {
		return
	}
	now := h.now()
	rows := make([]EventRow, 0, len(events))
	for _, it := range env.Items {
		if it.Header.Type != ItemEvent {
			continue
		}
		row, err := Prepare(p.HMACSecret, it.Payload, now)
		if err != nil {
			h.fail(w, http.StatusBadRequest, "bad event", err)
			return
		}
		if h.Samples != nil && !h.Samples.Allow(p.ID, row.Fingerprint, now) {
			row.Payload = nil
		}
		rows = append(rows, row)
	}
	rec := Received{
		Project: p, ProjectID: r.PathValue("project_id"), PublicKey: p.PublicKey,
		Raw: data, Envelope: env, Events: events, Rows: rows, Origin: r.Header.Get("Origin"),
	}
	if err := h.Sink.Envelope(r.Context(), rec); err != nil {
		h.fail(w, http.StatusServiceUnavailable, "unavailable", err)
		return
	}
	ack()
}

// PageviewRow is one page view ready to store.
type PageviewRow struct {
	TS          time.Time
	Route       string
	SessionID   string
	ReleaseID   string
	LoadMS      *int32
	VisitorHash string
	Country     string
	Device      string
	vid         string
}

type pageviewIn struct {
	Path      string   `json:"path"`
	SessionID string   `json:"session_id"`
	Release   string   `json:"release"`
	LoadMS    *float64 `json:"load_ms"`
	Device    string   `json:"device"`
	VID       string   `json:"vid"`
}

// maxPageviewsPerRequest bounds one batch.
const maxPageviewsPerRequest = 50

// parsePageviews accepts one object or an array of them.
func parsePageviews(data []byte, now time.Time) ([]PageviewRow, error) {
	var list []pageviewIn
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return nil, err
		}
	} else {
		var one pageviewIn
		if err := json.Unmarshal(trimmed, &one); err != nil {
			return nil, err
		}
		list = []pageviewIn{one}
	}
	if len(list) > maxPageviewsPerRequest {
		list = list[:maxPageviewsPerRequest]
	}
	rows := make([]PageviewRow, 0, len(list))
	for _, pv := range list {
		if pv.Path == "" {
			continue
		}
		row := PageviewRow{
			TS:        now, // server time: client clocks are untrusted
			Route:     truncate(normalize.RouteTemplate(pv.Path), 500),
			SessionID: truncate(pv.SessionID, 64),
			ReleaseID: truncate(pv.Release, 200),
			Device:    analytics.Device(pv.Device),
			vid:       truncate(pv.VID, 64),
		}
		if pv.LoadMS != nil && *pv.LoadMS >= 0 && *pv.LoadMS < 600_000 {
			v := int32(*pv.LoadMS)
			row.LoadMS = &v
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (h *Handler) pageview(w http.ResponseWriter, r *http.Request) {
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
	rows, err := parsePageviews(data, h.now())
	if err != nil {
		h.fail(w, http.StatusBadRequest, "bad json", nil)
		return
	}
	if h.Enrich != nil {
		ip := h.Enrich.ClientIP(r)
		country := h.Enrich.Country(ip)
		for i := range rows {
			rows[i].VisitorHash = h.Enrich.Visitor(p.HMACSecret, p.ID, rows[i].vid, ip, r.UserAgent(), rows[i].TS)
			rows[i].Country = country
		}
	}
	if len(rows) > 0 {
		rec := Received{Project: p, PublicKey: p.PublicKey, Raw: data, Pageviews: rows, Origin: r.Header.Get("Origin")}
		if err := h.Sink.Pageview(r.Context(), rec); err != nil {
			h.fail(w, http.StatusServiceUnavailable, "unavailable", err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

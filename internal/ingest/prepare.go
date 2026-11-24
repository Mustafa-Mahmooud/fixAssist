package ingest

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/yourname/fixassist/internal/normalize"
)

// Event kinds stored in events.kind.
const (
	KindJS      = "js"
	KindNet     = "net"
	KindMessage = "message"
)

// EventRow is one event ready to store: scrubbed, with derived columns.
type EventRow struct {
	ReceivedAt  time.Time
	Kind        string
	Fingerprint string
	ReleaseID   string
	Route       string
	UserHash    string
	Payload     []byte // nil when the sample cap dropped it
}

// UserHash is HMAC-SHA256(project secret, id), hex, first 32 chars.
// Raw user IDs are never stored.
func UserHash(secret []byte, id string) string {
	if id == "" {
		return ""
	}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(id))
	return hex.EncodeToString(m.Sum(nil))[:32]
}

func stringish(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

// identity returns the user ID the app set, else the add-on's per-tab session
// ID, and removes both from the payload.
func identity(m map[string]any) string {
	id := ""
	if u, ok := m["user"].(map[string]any); ok {
		id = stringish(u["id"])
		delete(u, "id")
		if len(u) == 0 {
			delete(m, "user")
		}
	}
	if c, ok := m["contexts"].(map[string]any); ok {
		if f, ok := c["fix"].(map[string]any); ok {
			if id == "" {
				id = stringish(f["session_id"])
			}
			delete(c, "fix")
		}
	}
	return id
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// JSError converts an event's main exception for fingerprinting.
func JSError(ev *Event) normalize.JSError {
	ex := ev.MainException()
	if ex == nil {
		return normalize.JSError{PageURL: ev.PageURL()}
	}
	e := normalize.JSError{Type: ex.Type, Message: ex.Value, PageURL: ev.PageURL()}
	if ex.Stacktrace != nil {
		for _, f := range ex.Stacktrace.Frames {
			fn := f.AbsPath
			if fn == "" {
				fn = f.Filename
			}
			e.Frames = append(e.Frames, normalize.Frame{Filename: fn, Function: f.Function, InApp: f.InApp})
		}
	}
	return e
}

// NetFailure converts the add-on's network context.
func NetFailure(h *HTTPContext) normalize.NetFailure {
	return normalize.NetFailure{Method: h.Method, URL: h.URL, Status: h.Status, Body: h.Body, Error: h.Error}
}

// Classify returns the event's kind and fingerprint.
func Classify(ev *Event) (kind, fingerprint string) {
	if h, ok := ev.HTTP(); ok {
		return KindNet, normalize.NetFingerprint(NetFailure(h))
	}
	if ev.MainException() != nil {
		return KindJS, normalize.JSFingerprintV2(JSError(ev))
	}
	return KindMessage, normalize.MessageFingerprint(ev.MessageText(), ev.PageURL())
}

// Prepare scrubs one event item, replaces the user with a hash and derives
// the stored columns. Derived fields come from the scrubbed payload, so what
// we fingerprint is exactly what we store.
func Prepare(secret []byte, item []byte, now time.Time) (EventRow, error) {
	dec := json.NewDecoder(bytes.NewReader(item))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return EventRow{}, fmt.Errorf("event: %w", err)
	}
	userHash := UserHash(secret, identity(m))
	ScrubEvent(m)
	if userHash != "" {
		u, _ := m["user"].(map[string]any)
		if u == nil {
			u = map[string]any{}
			m["user"] = u
		}
		u["hash"] = userHash
	}
	payload, err := json.Marshal(m)
	if err != nil {
		return EventRow{}, err
	}
	ev, err := ParseEvent(payload)
	if err != nil {
		return EventRow{}, err
	}
	kind, fp := Classify(ev)
	return EventRow{
		ReceivedAt:  now,
		Kind:        kind,
		Fingerprint: fp,
		ReleaseID:   truncate(ev.Release, 200),
		Route:       truncate(normalize.RouteTemplate(ev.PageURL()), 500),
		UserHash:    userHash,
		Payload:     payload,
	}, nil
}

// SampleCap keeps at most Limit stored payloads per project+fingerprint per
// hour (FDR quotas). Over the cap the row is still written, without payload,
// so counters stay right. In-memory: per process, reset each hour.
type SampleCap struct {
	Limit int

	mu     sync.Mutex
	hour   int64
	counts map[string]int
}

// DefaultSampleCap is the FDR's 100 stored events per fingerprint per hour.
const DefaultSampleCap = 100

// Allow reports whether this event's payload may be stored, and counts it.
func (c *SampleCap) Allow(projectID int64, fingerprint string, now time.Time) bool {
	limit := c.Limit
	if limit <= 0 {
		limit = DefaultSampleCap
	}
	h := now.Unix() / 3600
	c.mu.Lock()
	defer c.mu.Unlock()
	if h != c.hour || c.counts == nil {
		c.hour = h
		c.counts = map[string]int{}
	}
	k := fmt.Sprintf("%d|%s", projectID, fingerprint)
	if c.counts[k] >= limit {
		return false
	}
	c.counts[k]++
	return true
}

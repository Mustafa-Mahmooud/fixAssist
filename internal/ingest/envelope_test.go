package ingest

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const sampleEnvelope = `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","sent_at":"2026-11-22T10:00:00.000Z","sdk":{"name":"sentry.javascript.browser","version":"8.55.0"}}
{"type":"event"}
{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","level":"error","platform":"javascript","release":"fix-abc","exception":{"values":[{"type":"TypeError","value":"x is undefined","stacktrace":{"frames":[{"filename":"http://localhost:4173/assets/index-BwXk3Q9a.js","function":"onClick","lineno":1,"colno":2,"in_app":true}]}}]},"request":{"url":"http://localhost:4173/"},"tags":{"spike_case":"js_typeerror"}}
{"type":"session","length":13}
{"sid":"abc"}
{"type":"client_report"}
{"discarded_events":[]}
`

func TestParseEnvelope(t *testing.T) {
	env, err := ParseEnvelope([]byte(sampleEnvelope))
	if err != nil {
		t.Fatal(err)
	}
	if env.Header.EventID != "9ec79c33ec9942ab8353589fcb2e04dc" || env.Header.SDK.Version != "8.55.0" {
		t.Errorf("header = %+v", env.Header)
	}
	if len(env.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(env.Items))
	}
	if got := string(env.Items[1].Payload); got != `{"sid":"abc"}` {
		t.Errorf("length-delimited payload = %q", got)
	}
	evs, err := env.Events()
	if err != nil || len(evs) != 1 {
		t.Fatalf("events = %d, err = %v", len(evs), err)
	}
	ev := evs[0]
	ex := ev.MainException()
	if ex == nil || ex.Type != "TypeError" || len(ex.Stacktrace.Frames) != 1 {
		t.Fatalf("exception = %+v", ex)
	}
	if ev.Release != "fix-abc" || ev.Tags["spike_case"] != "js_typeerror" || ev.PageURL() != "http://localhost:4173/" {
		t.Errorf("event = %+v", ev)
	}
}

func TestParseEnvelopeNoTrailingNewlineAndLengthWithNewlines(t *testing.T) {
	data := "{}\n{\"type\":\"attachment\",\"length\":5}\na\nb\nc\n{\"type\":\"event\"}\n{\"event_id\":\"1\"}"
	env, err := ParseEnvelope([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Items) != 2 || string(env.Items[0].Payload) != "a\nb\nc" || string(env.Items[1].Payload) != `{"event_id":"1"}` {
		t.Fatalf("items = %+v", env.Items)
	}
}

func TestParseEnvelopeErrors(t *testing.T) {
	for _, bad := range []string{"not json\n", "{}\nnot json\n{}", "{}\n{\"type\":\"event\",\"length\":100}\nshort"} {
		if _, err := ParseEnvelope([]byte(bad)); err == nil {
			t.Errorf("ParseEnvelope(%q) succeeded", bad)
		}
	}
	if _, err := ParseEnvelope(bytes.Repeat([]byte("x"), MaxEnvelopeBytes+1)); err != ErrTooLarge {
		t.Errorf("oversize: err = %v", err)
	}
}

func TestTagsAsPairs(t *testing.T) {
	ev, err := ParseEvent([]byte(`{"tags":[["a","1"],["b",2]]}`))
	if err != nil || ev.Tags["a"] != "1" || ev.Tags["b"] != "2" {
		t.Fatalf("tags = %v, err = %v", ev.Tags, err)
	}
}

func TestPublicKey(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/1/envelope/?sentry_key=q&sentry_version=7", nil)
	if k := PublicKey(r, ""); k != "q" {
		t.Errorf("query key = %q", k)
	}
	r.Header.Set("X-Sentry-Auth", "Sentry sentry_key=h, sentry_version=7")
	if k := PublicKey(r, ""); k != "h" {
		t.Errorf("header key = %q", k)
	}
	r2 := httptest.NewRequest("POST", "/api/1/envelope/", nil)
	if k := PublicKey(r2, "http://d@localhost:8080/1"); k != "d" {
		t.Errorf("dsn key = %q", k)
	}
}

type memSink struct{ got []Received }

func (m *memSink) Envelope(_ context.Context, r Received) error { m.got = append(m.got, r); return nil }
func (m *memSink) Pageview(_ context.Context, r Received) error { m.got = append(m.got, r); return nil }
func (m *memSink) Analytics(_ context.Context, r Received) error {
	m.got = append(m.got, r)
	return nil
}

func TestHandler(t *testing.T) {
	sink := &memSink{}
	mux := http.NewServeMux()
	(&Handler{Sink: sink, Log: discardLogger()}).Register(mux)

	// Plain envelope with query auth.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/1/envelope/?sentry_key=k", strings.NewReader(sampleEnvelope)))
	if rec.Code != 200 || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("status %d headers %v body %s", rec.Code, rec.Header(), rec.Body)
	}
	if len(sink.got) != 1 || sink.got[0].ProjectID != "1" || len(sink.got[0].Events) != 1 {
		t.Fatalf("sink = %+v", sink.got)
	}

	// Gzip.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte(sampleEnvelope))
	_ = gz.Close()
	req := httptest.NewRequest("POST", "/api/1/envelope/?sentry_key=k", &buf)
	req.Header.Set("Content-Encoding", "gzip")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("gzip status %d: %s", rec.Code, rec.Body)
	}

	// Missing key.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/1/envelope/", strings.NewReader("{}\n")))
	if rec.Code != 401 {
		t.Errorf("no key: status %d", rec.Code)
	}

	// Oversize.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/1/envelope/?sentry_key=k", bytes.NewReader(bytes.Repeat([]byte("x"), MaxEnvelopeBytes+10))))
	if rec.Code != 413 {
		t.Errorf("oversize: status %d", rec.Code)
	}

	// Legacy store.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/1/store/?sentry_key=k", strings.NewReader(`{"event_id":"e1","message":"hi"}`)))
	if rec.Code != 200 {
		t.Errorf("store: status %d", rec.Code)
	}

	// Preflight.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("OPTIONS", "/api/1/envelope/", nil))
	if rec.Code != 204 {
		t.Errorf("preflight: status %d", rec.Code)
	}

	// Page view.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/pv?sentry_key=k", strings.NewReader(`{"path":"/"}`)))
	if rec.Code != 204 {
		t.Errorf("pv: status %d", rec.Code)
	}
}

// Every test error is one issue, whatever page or stack it came from.
func TestClassifyTestError(t *testing.T) {
	mk := func(page, file, msg string) *Event {
		var ev Event
		js := fmt.Sprintf(`{"request":{"url":%q},"exception":{"values":[{"type":"Error","value":%q,"stacktrace":{"frames":[{"filename":%q,"function":"f","in_app":true}]}}]}}`, page, msg, file)
		if err := json.Unmarshal([]byte(js), &ev); err != nil {
			t.Fatal(err)
		}
		return &ev
	}
	k1, f1 := Classify(mk("https://a.example/", "https://a.example/assets/x.js", "Fix Assistant test"))
	k2, f2 := Classify(mk("https://a.example/pricing/42", "https://ingest.example/sdk/k.js", "Fix Assistant test"))
	if k1 != KindJS || k2 != KindJS || f1 != f2 {
		t.Errorf("test errors: %s %s / %s %s", k1, f1, k2, f2)
	}
	if _, other := Classify(mk("https://a.example/", "https://a.example/assets/x.js", "Fix Assistant test 2")); other == f1 {
		t.Error("other errors must not share the test fingerprint")
	}
}

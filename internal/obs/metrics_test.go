package obs

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricsAccessAndCounts(t *testing.T) {
	m := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := m.Wrap("ingest.envelope", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) })
	h(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/1/envelope/", nil))
	h(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/1/envelope/", nil))
	if got := m.RequestCounts("ingest.")["429"]; got != 2 {
		t.Errorf("429 count = %v, want 2", got)
	}

	handler := m.Handler("s3cret")
	get := func(remote, authz string) int {
		r := httptest.NewRequest("GET", "/metrics", nil)
		r.RemoteAddr = remote
		if authz != "" {
			r.Header.Set("Authorization", authz)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code == 200 && !strings.Contains(w.Body.String(), `fix_http_requests_total{code="429",endpoint="ingest.envelope"} 2`) {
			t.Errorf("metrics body missing counter:\n%s", w.Body.String()[:300])
		}
		return w.Code
	}
	for _, c := range []struct {
		remote, authz string
		want          int
	}{
		{"127.0.0.1:5000", "", 200},
		{"203.0.113.9:5000", "", 404},
		{"203.0.113.9:5000", "Bearer wrong", 404},
		{"203.0.113.9:5000", "Bearer s3cret", 200},
	} {
		if got := get(c.remote, c.authz); got != c.want {
			t.Errorf("%s %q: %d, want %d", c.remote, c.authz, got, c.want)
		}
	}
}

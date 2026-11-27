package agentapi_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yourname/fixassist/internal/agentapi"
	"github.com/yourname/fixassist/internal/analytics"
)

func TestPairCodeFormat(t *testing.T) {
	code, hash, err := agentapi.NewPairCode()
	if err != nil || len(code) != 11 || code[5] != '-' || strings.ContainsAny(code, "01OIL") {
		t.Fatalf("code %q, %v", code, err)
	}
	if agentapi.HashPairCode(strings.ToLower(strings.ReplaceAll(code, "-", " "))) != hash {
		t.Error("typed variants of the code must match")
	}
}

// A code works once, within 15 minutes, and gives a working agent token.
func TestPair(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	pair := func(code string) (int, map[string]any) {
		t.Helper()
		resp, err := http.Post(f.srv.URL+"/agent/v1/pair", "application/json", strings.NewReader(`{"code":"`+code+`","hostname":"laptop<b>"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(b, &out)
		return resp.StatusCode, out
	}
	code, err := agentapi.CreatePairCode(ctx, f.pool, f.pid, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	status, out := pair(strings.ToLower(code))
	token, _ := out["token"].(string)
	if status != 200 || !strings.HasPrefix(token, "fa_") || out["project"] != "Shop" {
		t.Fatalf("pair: %d %v", status, out)
	}
	req, _ := http.NewRequest("GET", f.srv.URL+"/agent/v1/autofix/prs?hostname=laptop", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Errorf("the paired token doesn't work: %v %v", resp, err)
	}
	var name string
	_ = f.pool.QueryRow(ctx, "select t.name from agent_tokens t join pair_codes c on c.token_id = t.id").Scan(&name)
	if name != "fixassist on laptopb" {
		t.Errorf("token name = %q", name)
	}
	if status, out := pair(code); status != 404 || !strings.Contains(out["error"].(string), "already used") {
		t.Errorf("second use: %d %v", status, out)
	}
	old, _ := agentapi.CreatePairCode(ctx, f.pool, f.pid, 0, time.Now().Add(-agentapi.PairCodeTTL-time.Minute))
	if status, _ := pair(old); status != 404 {
		t.Errorf("expired code: %d", status)
	}
	// Guessing is cut off after a few tries a minute.
	limited := false
	for i := 0; i < 12; i++ {
		if status, _ := pair("AAAAA-AAAAA"); status == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Error("no rate limit on pairing")
	}
}

// Behind the proxy, each founder has their own budget of tries.
func TestPairLimitPerClient(t *testing.T) {
	f := setup(t)
	mux := http.NewServeMux()
	(&agentapi.Handler{Pool: f.pool, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ClientIP: (&analytics.Enricher{TrustProxy: true}).ClientIP}).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	try := func(ip string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/agent/v1/pair", strings.NewReader(`{"code":"AAAAA-AAAAA"}`))
		req.Header.Set("X-Forwarded-For", ip)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for i := 0; i < 12; i++ {
		try("198.51.100.1")
	}
	if got := try("198.51.100.1"); got != http.StatusTooManyRequests {
		t.Errorf("guesser: %d, want 429", got)
	}
	if got := try("203.0.113.9"); got != http.StatusNotFound {
		t.Errorf("someone else: %d, want 404 (wrong code, not rate limited)", got)
	}
}

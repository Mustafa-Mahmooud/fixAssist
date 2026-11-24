package ingest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Same cases the browser scrubber (pkg/sdk-addon/addon.js) handles.
func TestScrubString(t *testing.T) {
	cases := []struct{ in, want string }{
		{"no account for jane.doe@example.com", "no account for [email]"},
		{"token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abc_DEF-123 bad", "token [jwt] bad"},
		{"Authorization: Bearer abc.def-123", "Authorization: Bearer [token]"},
		{`{"password":"hunter2","user":"x"}`, `{"password":"[secret]","user":"x"}`},
		{"api_key=sk_live_123&x=1", "api_key=[secret]&x="},
		{"password=hunter2", "password=[secret]"},
		// Captured from hosted GoTrue: a key ending in "password" holding an object
		// must survive, or the body stops being JSON and the code is lost.
		{`{"code":"weak_password","message":"Password should be at least 6 characters.","weak_password":{"reasons":["length"]}}`,
			`{"code":"weak_password","message":"Password should be at least 6 characters.","weak_password":{"reasons":["length"]}}`},
		{`{"token_type":"bearer","access_token":"abc"}`, `{"token_type":"bearer","access_token":"[secret]"}`},
		{`{"refresh_token":{"x":1}}`, `{"refresh_token":{"x":1}}`},
		{"card 4242 4242 4242 4242 declined", "card [card] declined"},
		{"order 1795381381069 failed", "order 1795381381069 failed"}, // not Luhn-valid
		{"https://a.supabase.co/rest/v1/users?select=*&id=eq.5", "https://a.supabase.co/rest/v1/users?select=&id="},
		{"value too long for type character varying(20)", "value too long for type character varying(20)"},
	}
	for _, c := range cases {
		if got := ScrubString(c.in); got != c.want {
			t.Errorf("ScrubString(%q)\n got  %q\n want %q", c.in, got, c.want)
		}
	}
}

func TestTruncateIP(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.77":        "203.0.113.0",
		"2001:db8:abcd:12::1": "2001:db8:abcd::",
		"{{auto}}":            "",
	} {
		if got := TruncateIP(in); got != want {
			t.Errorf("TruncateIP(%q) = %q, want %q", in, got, want)
		}
	}
}

const rawEvent = `{
  "event_id": "abc",
  "release": "fix-0bdd6fbc7666354a",
  "exception": {"values": [{"type": "TypeError", "value": "x is undefined for bob@example.com",
    "stacktrace": {"frames": [{"filename": "http://localhost:4173/assets/index-DpRClB3D.js", "function": "lf"}]}}]},
  "request": {"url": "http://localhost:4173/checkout?coupon=SAVE10", "cookies": "sid=1",
    "headers": {"User-Agent": "x", "Authorization": "Bearer secret", "Cookie": "a=b"}, "data": {"card": "4242"}},
  "user": {"id": "user-42", "email": "bob@example.com", "ip_address": "203.0.113.77"},
  "contexts": {"fix": {"session_id": "tab1"}},
  "breadcrumbs": {"values": [{"category": "ui.input", "message": "typed"}, {"category": "ui.click", "message": "button"}]}
}`

func TestPrepare(t *testing.T) {
	secret := []byte("s3cret")
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	row, err := Prepare(secret, []byte(rawEvent), now)
	if err != nil {
		t.Fatal(err)
	}
	if row.Kind != KindJS || row.Route != "/checkout" || row.ReleaseID != "fix-0bdd6fbc7666354a" || !strings.HasPrefix(row.Fingerprint, "v2:") {
		t.Errorf("row = %+v", row)
	}
	if row.UserHash != UserHash(secret, "user-42") || len(row.UserHash) != 32 {
		t.Errorf("user hash = %q", row.UserHash)
	}
	p := string(row.Payload)
	for _, bad := range []string{"bob@example.com", "user-42", "tab1", "Bearer secret", "sid=1", "a=b", "4242", "SAVE10", "typed", "203.0.113.77"} {
		if strings.Contains(p, bad) {
			t.Errorf("payload still contains %q: %s", bad, p)
		}
	}
	var m map[string]any
	_ = json.Unmarshal(row.Payload, &m)
	u := m["user"].(map[string]any)
	if u["hash"] != row.UserHash || u["ip_address"] != "203.0.113.0" {
		t.Errorf("user = %v", u)
	}
	if strings.Count(p, "ui.click") != 1 {
		t.Errorf("click breadcrumb lost: %s", p)
	}

	// Same error, different user and scrub state: same fingerprint.
	other := strings.Replace(rawEvent, "user-42", "user-7", 1)
	row2, _ := Prepare(secret, []byte(other), now)
	if row2.Fingerprint != row.Fingerprint || row2.UserHash == row.UserHash {
		t.Error("fingerprint should ignore the user; user hash should not")
	}

	// No user id: the session id is the identity.
	anon := strings.Replace(rawEvent, `"id": "user-42", `, "", 1)
	row3, _ := Prepare(secret, []byte(anon), now)
	if row3.UserHash != UserHash(secret, "tab1") {
		t.Errorf("anonymous hash = %q", row3.UserHash)
	}

	// Different project secret: different hash for the same user.
	row4, _ := Prepare([]byte("other"), []byte(rawEvent), now)
	if row4.UserHash == row.UserHash {
		t.Error("user hash must be per project")
	}
}

func TestPrepareKinds(t *testing.T) {
	net := `{"exception":{"values":[{"type":"HTTPError","value":"POST /rest/v1/x 400"}]},
	  "contexts":{"fix_http":{"method":"POST","url":"https://a.supabase.co/rest/v1/x","status":400,"body":"{\"code\":\"22001\",\"details\":null,\"hint\":null,\"message\":\"m\"}"}}}`
	msg := `{"message":"hello 12345","request":{"url":"https://a.com/p"}}`
	for in, want := range map[string]string{net: KindNet, msg: KindMessage} {
		row, err := Prepare(nil, []byte(in), time.Now())
		if err != nil || row.Kind != want || row.UserHash != "" {
			t.Errorf("kind = %q (want %q), hash %q, err %v", row.Kind, want, row.UserHash, err)
		}
	}
}

func TestSampleCap(t *testing.T) {
	c := &SampleCap{Limit: 2}
	now := time.Date(2026, 11, 22, 10, 5, 0, 0, time.UTC)
	got := []bool{c.Allow(1, "fp", now), c.Allow(1, "fp", now), c.Allow(1, "fp", now), c.Allow(2, "fp", now), c.Allow(1, "fp2", now)}
	want := []bool{true, true, false, true, true}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Allow #%d = %v, want %v", i, got[i], want[i])
		}
	}
	if !c.Allow(1, "fp", now.Add(time.Hour)) {
		t.Error("cap should reset next hour")
	}
}

func TestParsePageviews(t *testing.T) {
	now := time.Now()
	rows, err := parsePageviews([]byte(`[{"path":"/orders/12345","session_id":"s","release":"r","load_ms":120.4},{"path":""}]`), now)
	if err != nil || len(rows) != 1 || rows[0].Route != "/orders/<id>" || *rows[0].LoadMS != 120 {
		t.Fatalf("rows = %+v, err %v", rows, err)
	}
	rows, _ = parsePageviews([]byte(`{"path":"/","load_ms":-5}`), now)
	if len(rows) != 1 || rows[0].LoadMS != nil {
		t.Fatalf("rows = %+v", rows)
	}
}

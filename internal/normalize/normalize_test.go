package normalize

import "testing"

func TestMessage(t *testing.T) {
	cases := []struct{ in, want string }{
		{"value too long for type character varying(20)", "value too long for type character varying(20)"},
		{"Checkout failed for order 1732291200123", "Checkout failed for order <n>"},
		{"user 3f2504e0-4f89-11d3-9a0c-0305e82c3301 not found", "user <uuid> not found"},
		{"no account for jane.doe@example.com", "no account for <email>"},
		{"Cannot read properties of null (reading 'profile')", "Cannot read properties of null (reading <str>)"},
		{`relation "public.missing" does not exist`, "relation <str> does not exist"},
		{"token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abc_DEF-123 expired", "token <jwt> expired"},
		{"bad id deadbeefcafe1234", "bad id <hex>"},
		{"  many   spaces\n here ", "many spaces here"},
		{"only 42 left", "only 42 left"},
		// Browser-scrubbed and unscrubbed values must normalize identically.
		{"Checkout failed for order [card]", "Checkout failed for order <n>"},
		{"no account for [email]", "no account for <email>"},
		{"Authorization: Bearer [token]", "Authorization: Bearer <token>"},
	}
	for _, c := range cases {
		if got := Message(c.in); got != c.want {
			t.Errorf("Message(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestURLTemplate(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://abc.supabase.co/rest/v1/users?select=*&id=eq.5", "/rest/v1/users?id&select"},
		{"http://127.0.0.1:54321/rest/v1/profiles?columns=%22email%22", "/rest/v1/profiles?columns"},
		{"https://app.example.com/orders/12345/items", "/orders/<id>/items"},
		{"https://app.example.com/u/3f2504e0-4f89-11d3-9a0c-0305e82c3301", "/u/<id>"},
		{"https://app.example.com/p/aB3dE5fG7h", "/p/<id>"},
		{"https://app.example.com/checkout", "/checkout"},
		{"https://app.example.com/", "/"},
		{"https://app.example.com", "/"},
		{"/auth/v1/token?grant_type=password", "/auth/v1/token?grant_type"},
	}
	for _, c := range cases {
		if got := URLTemplate(c.in); got != c.want {
			t.Errorf("URLTemplate(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFilename(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://app.lovable.app/assets/index-BwXk3Q9a.js", "/assets/index.js"},
		{"http://localhost:4173/assets/index-D-x3_k9A.js:12:345", "/assets/index.js"},
		{"https://x.com/static/js/main.4f3a2b1c.js", "/static/js/main.js"},
		{"https://x.com/assets/index-a1b2c3.js", "/assets/index-a1b2c3.js"}, // 6 chars: not a Vite hash
		{"https://x.com/assets/my-component.js", "/assets/my-component.js"},
		{"https://x.com/assets/vendor-chunkies.js?v=3", "/assets/vendor-chunkies.js"},
		{"https://x.com/assets/index-ABCDEFGH.js", "/assets/index-ABCDEFGH.js"}, // all upper, no digit
	}
	for _, c := range cases {
		if got := Filename(c.in); got != c.want {
			t.Errorf("Filename(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLooksMinified(t *testing.T) {
	for fn, want := range map[string]bool{"": true, "?": true, "a": true, "Hd": true, "Object.Xe": true, "onClick": false, "HTMLButtonElement.onClick": false, "abc": false} {
		if got := LooksMinified(fn); got != want {
			t.Errorf("LooksMinified(%q) = %v, want %v", fn, got, want)
		}
	}
}

func TestJSFingerprintIgnoresBundleHashAndLines(t *testing.T) {
	page := "https://app.example.com/checkout"
	mk := func(file, fn string) JSError {
		return JSError{Type: "TypeError", Message: "x is undefined", PageURL: page,
			Frames: []Frame{{Filename: "https://app.example.com/assets/" + file, Function: fn}}}
	}
	a := mk("index-BwXk3Q9a.js", "onClick")
	b := mk("index-Zz91Qp0e.js", "onClick")
	for _, rule := range []FrameRule{RuleDropMinified, RuleFileOnly} {
		if JSFingerprint(a, rule) != JSFingerprint(b, rule) {
			t.Errorf("%s: bundle hash changed the fingerprint", rule)
		}
	}
	// Minified names differ between builds: drop-minified ignores them, file-only never used them.
	c, d := mk("index-BwXk3Q9a.js", "Xe"), mk("index-Zz91Qp0e.js", "Qr")
	for _, rule := range []FrameRule{RuleDropMinified, RuleFileOnly} {
		if JSFingerprint(c, rule) != JSFingerprint(d, rule) {
			t.Errorf("%s: minified function names changed the fingerprint", rule)
		}
	}
	// Different message => different issue.
	e := mk("index-BwXk3Q9a.js", "onClick")
	e.Message = "y is undefined"
	if JSFingerprint(a, DefaultFrameRule) == JSFingerprint(e, DefaultFrameRule) {
		t.Error("different messages share a fingerprint")
	}
}

func TestFramesFromOtherOriginsIgnored(t *testing.T) {
	page := "https://app.example.com/"
	e := JSError{Type: "Error", Message: "boom", PageURL: page, Frames: []Frame{
		{Filename: "https://app.example.com/assets/index-BwXk3Q9a.js", Function: "handleBuy"},
		{Filename: "chrome-extension://abc/content.js", Function: "inject"},
	}}
	if got := FramesKey(e, RuleDropMinified); got != "/assets/index.js:handleBuy" {
		t.Errorf("FramesKey = %q", got)
	}
}

func TestNetFingerprintAndSignature(t *testing.T) {
	body := `{"code":"22001","details":null,"hint":null,"message":"value too long for type character varying(20)"}`
	n1 := NetFailure{Method: "post", URL: "https://a.supabase.co/rest/v1/users?columns=%22email%22", Status: 400, Body: body}
	n2 := n1
	n2.URL = "https://a.supabase.co/rest/v1/users?columns=%22email%22,%22name%22"
	if NetFingerprint(n1) != NetFingerprint(n2) {
		t.Error("query values changed the network fingerprint")
	}
	n3 := n1
	n3.Body = `{"code":"42501","details":null,"hint":null,"message":"new row violates row-level security policy for table \"users\""}`
	if NetFingerprint(n1) == NetFingerprint(n3) {
		t.Error("different PostgREST codes share a fingerprint")
	}
	want := `net POST /rest/v1/users?columns 400 pg:22001 "value too long for type character varying(20)" platform=lovable stack=supabase`
	if got := NetSignature(n1, Tags{Platform: "lovable", Stack: "supabase"}); got != want {
		t.Errorf("NetSignature =\n %s\nwant\n %s", got, want)
	}
	dead := NetFailure{Method: "GET", URL: "http://127.0.0.1:9/ping", Status: 0, Error: "Failed to fetch"}
	if got := NetSignature(dead, Tags{}); got != `net GET /ping 0 "Failed to fetch"` {
		t.Errorf("dead signature = %q", got)
	}
}

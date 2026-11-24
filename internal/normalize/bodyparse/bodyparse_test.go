package bodyparse

import "testing"

func TestParse(t *testing.T) {
	const rest = "https://x.supabase.co/rest/v1/profiles"
	const authURL = "https://x.supabase.co/auth/v1/token?grant_type=password"
	cases := []struct {
		name, url, body    string
		status             int
		backend, code, msg string
	}{
		{"postgrest 22001", rest, `{"code":"22001","details":null,"hint":null,"message":"value too long for type character varying(20)"}`, 400,
			"postgrest", "22001", "value too long for type character varying(20)"},
		{"postgrest 42501", rest, `{"code":"42501","details":null,"hint":null,"message":"new row violates row-level security policy for table \"secrets\""}`, 401,
			"postgrest", "42501", `new row violates row-level security policy for table "secrets"`},
		{"postgrest PGRST205", rest, `{"code":"PGRST205","details":null,"hint":"Perhaps you meant the table 'public.profiles'","message":"Could not find the table 'public.missing' in the schema cache"}`, 404,
			"postgrest", "PGRST205", "Could not find the table 'public.missing' in the schema cache"},
		{"gotrue error_code", authURL, `{"code":400,"error_code":"invalid_credentials","msg":"Invalid login credentials"}`, 400,
			"supabase_auth", "invalid_credentials", "Invalid login credentials"},
		// Captured from hosted Supabase in the Phase 1 spike.
		{"gotrue hosted", authURL, `{"code":"invalid_credentials","message":"Invalid login credentials"}`, 400,
			"supabase_auth", "invalid_credentials", "Invalid login credentials"},
		{"same shape off auth", "https://api.example.com/orders", `{"code":"invalid_credentials","message":"Invalid login credentials"}`, 400,
			"json", "invalid_credentials", "Invalid login credentials"},
		{"gotrue oauth", authURL, `{"error":"invalid_grant","error_description":"Invalid login credentials"}`, 400,
			"supabase_auth", "invalid_grant", "Invalid login credentials"},
		{"firebase", "", `{"error":{"code":400,"message":"EMAIL_EXISTS","status":"INVALID_ARGUMENT"}}`, 400,
			"firebase", "EMAIL_EXISTS", "EMAIL_EXISTS"},
		{"generic", "", `{"error":"Not allowed","code":"E_FORBIDDEN"}`, 403, "json", "E_FORBIDDEN", "Not allowed"},
		{"generic nested", "", `{"error":{"message":"quota exceeded","code":429}}`, 429, "json", "429", "quota exceeded"},
		{"raw html", "", `<html><body>502 Bad Gateway</body></html>`, 502, "raw", "", "<html><body>502 Bad Gateway</body></html>"},
		{"truncated json", rest, `{"code":"22001","details":null,"hint":null,"mess`, 400, "raw", "", `{"code":"22001","details":null,"hint":null,"mess`},
		{"empty", "", ``, 500, "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Parse(Input{Status: c.status, URL: c.url, Body: []byte(c.body)})
			if r.Backend != c.backend || r.Code != c.code || r.Message != c.msg {
				t.Errorf("got %+v, want backend=%q code=%q msg=%q", r, c.backend, c.code, c.msg)
			}
		})
	}
}

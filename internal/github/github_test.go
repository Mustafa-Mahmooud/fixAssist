package github

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yourname/fixassist/internal/ai"
)

// FakeRepo builds a gzipped GitHub-style tarball from path → content.
func FakeRepo(t testing.TB, files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for p, c := range files {
		if err := tw.WriteHeader(&tar.Header{Name: "acme-app-abc123/" + p, Mode: 0o644, Size: int64(len(c)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(c))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestJWTVerifies(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	a := &App{ID: 42, Key: key}
	tok, err := a.JWT(time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt = %s", tok)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature: %v", err)
	}
	claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c map[string]any
	_ = json.Unmarshal(claims, &c)
	if c["iss"] != "42" || c["exp"].(float64)-c["iat"].(float64) != 600 {
		t.Errorf("claims = %v", c)
	}
}

func TestScanFindsAndRanks(t *testing.T) {
	repo := FakeRepo(t, map[string]string{
		"src/pages/Signup.tsx":                  "import { supabase } from '../lib/supabase'\n\nexport async function save(email) {\n  await supabase.from(\"profiles\").insert({ email })\n}\n",
		"supabase/migrations/20260101_init.sql": "create table public.profiles (\n  id bigint primary key,\n  email varchar(20)\n);\n",
		"node_modules/lib/index.js":             "supabase.from('profiles')",
		"src/big.min.js":                        "supabase.from('profiles')",
		"README.md":                             "supabase.from('profiles')",
	})
	terms := ai.CodeTerms(&ai.Context{Net: []ai.NetSample{{Method: "POST", Path: "/rest/v1/profiles", Backend: "postgrest", Code: "22001"}}}, "pg.value_too_long")
	refs, err := Scan(repo, terms)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("refs = %+v", refs)
	}
	if refs[0].Path != "src/pages/Signup.tsx" || refs[0].Line != 4 || !strings.Contains(refs[0].Snippet, `supabase.from("profiles").insert`) {
		t.Errorf("code ref = %+v", refs[0])
	}
	if refs[1].Path != "supabase/migrations/20260101_init.sql" || refs[1].Line != 1 {
		t.Errorf("migration ref = %+v", refs[1])
	}
}

func TestManifest(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal(Manifest("https://fix.example.com"), &m); err != nil {
		t.Fatal(err)
	}
	perms := m["default_permissions"].(map[string]any)
	for name, level := range perms {
		if level != "read" {
			t.Errorf("permission %s = %v: the app must only ever read", name, level)
		}
	}
	if !strings.Contains(fmt.Sprint(m["callback_urls"]), "https://fix.example.com/auth/github/callback") {
		t.Errorf("callback_urls = %v, want the sign-in callback", m["callback_urls"])
	}
	if perms["contents"] != "read" || perms["email_addresses"] != "read" || len(perms) != 3 || m["request_oauth_on_install"] != true || m["setup_url"] != "https://fix.example.com/github/setup" {
		t.Errorf("manifest = %v", m)
	}
}

func TestManifestNameFitsGitHub(t *testing.T) {
	var a, b map[string]any
	_ = json.Unmarshal(Manifest("https://p01--fixit--yjtpmslhxt6q.code.run"), &a)
	_ = json.Unmarshal(Manifest("https://fix.example.com"), &b)
	na, nb := a["name"].(string), b["name"].(string)
	if len(na) > 34 || !strings.HasPrefix(na, "Fix Assistant ") {
		t.Errorf("name %q: GitHub allows at most 34 characters", na)
	}
	if na == nb {
		t.Error("two deployments get the same (global) app name")
	}
}

func TestSealOpen(t *testing.T) {
	sealed, err := seal([]byte("secret-one-secret-one-secret-one"), []byte("-----BEGIN KEY-----"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "BEGIN") {
		t.Fatal("not encrypted")
	}
	plain, err := open([]byte("secret-one-secret-one-secret-one"), sealed)
	if err != nil || string(plain) != "-----BEGIN KEY-----" {
		t.Fatalf("open = %q, %v", plain, err)
	}
	if _, err := open([]byte("another-secret-another-secret-00"), sealed); err == nil {
		t.Error("a different APP_SECRET decrypted it")
	}
}

// GitHub's own 5xx errors on the conversion are retried.
func TestConvertRetriesServerErrors(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(502)
			fmt.Fprint(w, `{"message":"Server Error"}`)
			return
		}
		fmt.Fprint(w, `{"id":7,"slug":"fix-assistant-x","client_id":"c","client_secret":"s","pem":"p"}`)
	}))
	defer srv.Close()
	c, err := Convert(context.Background(), srv.URL, "code")
	if err != nil || c.ID != 7 || calls != 2 {
		t.Fatalf("Convert = %+v, %v after %d calls", c, err, calls)
	}
	// A 4xx (a used or bad code) isn't retried.
	calls = 0
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(404) }))
	defer bad.Close()
	if _, err := Convert(context.Background(), bad.URL, "code"); err == nil || calls != 1 {
		t.Errorf("4xx: err %v after %d calls", err, calls)
	}
}

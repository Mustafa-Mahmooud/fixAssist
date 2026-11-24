package github

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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
	if perms["contents"] != "read" || len(perms) != 2 || m["request_oauth_on_install"] != true || m["setup_url"] != "https://fix.example.com/github/setup" {
		t.Errorf("manifest = %v", m)
	}
}

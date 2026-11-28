package daemonver

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

func TestSignAndOpen(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	bin := []byte("the new fixassist")
	sig := Sign(priv, "fixassist-windows-amd64.exe", "0.4.1", bin)

	m, err := open(sig, pubB64)
	if err != nil || m.File != "fixassist-windows-amd64.exe" || m.Version != "0.4.1" || !m.Matches(bin) || m.Matches([]byte("another")) {
		t.Fatalf("open: %+v %v", m, err)
	}
	// Another key, or any change to the signed line, fails.
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := open(sig, base64.StdEncoding.EncodeToString(other)); err != ErrBadSignature {
		t.Errorf("another key: %v", err)
	}
	for _, tamper := range []string{
		strings.Replace(string(sig), "0.4.1", "9.9.9", 1),                                       // a newer version
		strings.Replace(string(sig), "fixassist-windows-amd64.exe", "fixassist-linux-amd64", 1), // another platform
		strings.Replace(string(sig), m.SHA256, strings.Repeat("0", 64), 1),                      // another binary
		strings.SplitN(string(sig), "\n", 2)[0],                                                 // no signature
		"",
	} {
		if _, err := open([]byte(tamper), pubB64); err == nil {
			t.Errorf("accepted a tampered manifest: %q", tamper)
		}
	}
	if _, err := open(sig, ""); err == nil {
		t.Error("a build without a key accepted an update")
	}
}

func TestBuiltInKey(t *testing.T) {
	if !HasKey() {
		t.Fatal("this build has no update key: daemons could never update")
	}
}

func TestOlder(t *testing.T) {
	for _, c := range []struct {
		v, than string
		want    bool
	}{
		{"0.3.0", "0.4.0", true}, {"0.2.1", "0.4.0", true}, {"0.4.0", "0.4.0", false}, {"0.4.1", "0.4.0", false},
		{"0.10.0", "0.9.0", false}, {"0.9.0", "0.10.0", true}, {"1.0.0", "0.99.99", false},
		{"test", "0.4.0", false}, {"", "0.4.0", false}, {"0.4", "0.4.0", false}, {"0.4.0", "x", false}, {"-1.0.0", "0.4.0", false},
	} {
		if got := Older(c.v, c.than); got != c.want {
			t.Errorf("Older(%q, %q) = %v", c.v, c.than, got)
		}
	}
}

func TestFileName(t *testing.T) {
	if FileName("windows", "amd64") != "fixassist-windows-amd64.exe" || FileName("darwin", "arm64") != "fixassist-darwin-arm64" {
		t.Error("names don't match the Dockerfile's")
	}
}

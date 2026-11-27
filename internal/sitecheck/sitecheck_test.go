package sitecheck

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testKey    = "0123456789abcdef0123456789abcdef"
	testIngest = "https://ingest.fixassist.test"
	ourTag     = `<script data-fix-sdk src="https://ingest.fixassist.test/sdk/` + testKey + `.js"></script>`
)

func TestCheckHTML(t *testing.T) {
	cases := []struct {
		name, html, level, title string
	}{
		{"installed first", `<html><head><meta charset="utf-8">` + ourTag + `<script type="module" src="/assets/index.js"></script></head>`, "good", "on your live site"},
		{"data scripts before are fine", `<head><script type="application/ld+json">{}</script>` + ourTag + `</head>`, "good", "on your live site"},
		{"after a Vite module script", `<head><script type="module" crossorigin src="/assets/index-abc.js"></script>` + ourTag + `</head>`, "good", "on your live site"},
		{"after a deferred script", `<head><script defer src="/a.js"></script>` + ourTag + `</head>`, "good", "on your live site"},
		{"after an async module", `<head><script type="module" async src="/a.js"></script>` + ourTag + `</head>`, "warn", "other scripts load first"},
		{"after an inline script", `<head><script>window.x = 1</script>` + ourTag + `</head>`, "warn", "other scripts load first"},
		{"after other scripts", `<head><script src="/assets/index.js"></script>` + ourTag + `</head>`, "warn", "other scripts load first"},
		{"missing", `<head><script src="/assets/index.js"></script></head><body><div id="root"></div></body>`, "bad", "isn't on your live site"},
		{"other key", strings.Replace(ourTag, testKey, "ffffffffffffffffffffffffffffffff", 1), "bad", "different app"},
		{"other server", strings.Replace(ourTag, "ingest.fixassist.test", "old.example.com", 1), "bad", "wrong server"},
	}
	for _, c := range cases {
		got := HTML(c.html, testIngest, testKey, "Lovable")
		if got.Level != c.level || !strings.Contains(got.Title, c.title) {
			t.Errorf("%s: got %s %q", c.name, got.Level, got.Title)
		}
	}
}

func TestPublicIP(t *testing.T) {
	for ip, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "192.168.1.1": false, "172.16.0.1": false, "169.254.169.254": false,
		"100.64.1.1": false, "0.0.0.0": false, "::1": false, "fd00::1": false, "fe80::1": false, "::ffff:127.0.0.1": false,
	} {
		if got := PublicIP(net.ParseIP(ip)); got != want {
			t.Errorf("PublicIP(%s) = %v, want %v", ip, got, want)
		}
	}
}

// The real client must refuse to connect to this machine, even through a redirect.
func TestSiteClientRefusesPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "secret") }))
	defer srv.Close()
	_, err := Client().Get(srv.URL)
	if !errors.Is(err, ErrNotPublic) {
		t.Fatalf("fetching %s: err = %v, want ErrNotPublic", srv.URL, err)
	}
}

package analytics

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVisitorHash(t *testing.T) {
	e := &Enricher{Secret: []byte("s")}
	day1 := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	a := e.Visitor([]byte("p"), 1, "", "203.0.113.5", "UA", day1)
	if a != e.Visitor([]byte("p"), 1, "", "203.0.113.5", "UA", day1.Add(5*time.Hour)) {
		t.Error("same person, same day: hash must match")
	}
	if a == e.Visitor([]byte("p"), 1, "", "203.0.113.5", "UA", day1.Add(24*time.Hour)) {
		t.Error("next day must give a new hash (no cross-day tracking)")
	}
	if a == e.Visitor([]byte("p"), 2, "", "203.0.113.5", "UA", day1) {
		t.Error("hash must differ per project")
	}
	v := e.Visitor([]byte("p"), 1, "vid-1", "203.0.113.5", "UA", day1)
	if v[:2] != "v:" || v != e.Visitor([]byte("p"), 1, "vid-1", "198.51.100.1", "other", day1.Add(72*time.Hour)) {
		t.Error("consented id must be stable across days and networks")
	}
}

func TestClientIPAndCountry(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.2:4000"
	r.Header.Set("X-Forwarded-For", "8.8.8.8, 10.0.0.2")
	if ip := (&Enricher{}).ClientIP(r); ip != "10.0.0.2" {
		t.Errorf("untrusted proxy header used: %s", ip)
	}
	if ip := (&Enricher{TrustProxy: true}).ClientIP(r); ip != "8.8.8.8" {
		t.Errorf("behind proxy: %s", ip)
	}
	db := filepath.Join(os.Getenv("LOCALAPPDATA"), "fixassist", "geo", "dbip.mmdb")
	if p := os.Getenv("GEOIP_DB"); p != "" {
		db = p
	}
	if _, err := os.Stat(db); err != nil {
		t.Skip("no GeoIP database")
	}
	e := &Enricher{}
	if err := e.OpenGeo(db); err != nil {
		t.Fatal(err)
	}
	if c := e.Country("8.8.8.8"); c != "US" {
		t.Errorf("8.8.8.8 country = %q", c)
	}
	if c := e.Country("not-an-ip"); c != "" {
		t.Errorf("bad ip country = %q", c)
	}
}

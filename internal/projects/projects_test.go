package projects

import (
	"testing"
	"time"
)

func TestOriginAllowed(t *testing.T) {
	list := []string{"myapp.lovable.app", "example.com"}
	cases := map[string]bool{
		"https://myapp.lovable.app":      true,
		"https://www.example.com":        true,
		"http://example.com:8080":        true,
		"https://evil-example.com":       false,
		"https://other.lovable.app":      false,
		"":                               false,
		"https://myapp.lovable.app.evil": false,
	}
	for origin, want := range cases {
		if got := OriginAllowed(list, origin); got != want {
			t.Errorf("OriginAllowed(%q) = %v, want %v", origin, got, want)
		}
	}
	if !OriginAllowed(nil, "") {
		t.Error("empty list must allow everything")
	}
}

func TestDSN(t *testing.T) {
	got, err := DSN("https://ingest.example.com/", "abc", 7)
	if err != nil || got != "https://abc@ingest.example.com/7" {
		t.Fatalf("DSN = %q, %v", got, err)
	}
}

func TestLimitsRate(t *testing.T) {
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	l := &Limits{Rate: 1, Burst: 3, Now: func() time.Time { return now }}
	p := Project{ID: 1, PublicKey: "k"}
	if d := l.Allow(p, 3); !d.Allowed {
		t.Fatal("burst should allow 3")
	}
	d := l.Allow(p, 1)
	if d.Allowed || d.Reason != "rate" || d.RetryAfter != time.Second {
		t.Fatalf("over rate: %+v", d)
	}
	now = now.Add(2 * time.Second)
	if d := l.Allow(p, 2); !d.Allowed {
		t.Fatalf("refilled: %+v", d)
	}
	if l.Usage(1) != 5 {
		t.Errorf("usage = %d, want 5 (rejected events not counted)", l.Usage(1))
	}
}

func TestLimitsMonthly(t *testing.T) {
	now := time.Date(2026, 11, 30, 23, 0, 0, 0, time.UTC)
	l := &Limits{Rate: 1000, Burst: 1000, Now: func() time.Time { return now }}
	p := Project{ID: 9, PublicKey: "k", QuotaMonthly: 10}
	l.SeedUsage(MonthStart(now), map[int64]int64{9: 8})
	if d := l.Allow(p, 2); !d.Allowed {
		t.Fatal("8+2 should fit a quota of 10")
	}
	d := l.Allow(p, 1)
	if d.Allowed || d.Reason != "monthly" || d.RetryAfter != time.Hour {
		t.Fatalf("over monthly: %+v", d)
	}
	// Seeding a lower DB value never lowers the local count.
	l.SeedUsage(MonthStart(now), map[int64]int64{9: 3})
	if l.Usage(9) != 10 {
		t.Errorf("usage = %d", l.Usage(9))
	}
	now = now.Add(2 * time.Hour) // December
	if d := l.Allow(p, 1); !d.Allowed {
		t.Fatal("new month should reset")
	}
}

package web

import (
	"net/http/httptest"
	"testing"
)

func TestPager(t *testing.T) {
	for _, c := range []struct {
		page, total         int
		wantPage, pages     int
		first, last, offset int
		prev, next          bool
	}{
		{1, 0, 1, 1, 0, 0, 0, false, false},
		{1, 7, 1, 1, 1, 7, 0, false, false},
		{1, 43, 1, 5, 1, 10, 0, false, true},
		{2, 43, 2, 5, 11, 20, 10, true, true},
		{5, 43, 5, 5, 41, 43, 40, true, false},
		{99, 43, 5, 5, 41, 43, 40, true, false}, // past the end
		{-3, 43, 1, 5, 1, 10, 0, false, true},   // garbage
		{2, 20, 2, 2, 11, 20, 10, true, false},  // exact fit
	} {
		p := newPager(c.page, c.total, 10)
		if p.Page != c.wantPage || p.Pages != c.pages || p.Total != c.total || p.First() != c.first || p.Last() != c.last ||
			p.Offset() != c.offset || p.HasPrev() != c.prev || p.HasNext() != c.next {
			t.Errorf("newPager(%d, %d) = page %d of %d, %d–%d, off %d, prev %v, next %v",
				c.page, c.total, p.Page, p.Pages, p.First(), p.Last(), p.Offset(), p.HasPrev(), p.HasNext())
		}
	}
}

func TestPagerLinks(t *testing.T) {
	// Other parameters (a tab, another list's page) are kept; page 1 drops
	// the parameter.
	r := httptest.NewRequest("GET", "/admin?apps=3&quarantined=2&x=y", nil)
	p := pageOf(r, "quarantined", 60, 25, "quarantined", true)
	if p.Page != 2 || p.PrevURL() != "?apps=3&x=y#quarantined" || p.NextURL() != "?apps=3&quarantined=3&x=y#quarantined" {
		t.Errorf("links: page %d prev %q next %q", p.Page, p.PrevURL(), p.NextURL())
	}
	if p.PrevLabel() != "Newer" || p.NextLabel() != "Older" {
		t.Error("a newest-first list says Newer/Older")
	}
	// Asked for page 3 of 2: the last page. No anchor: back to the top.
	q := pageOf(r, "apps", 60, 50, "", false)
	if q.Page != 2 || q.HasNext() || q.PrevURL() != "?quarantined=2&x=y" || q.PrevLabel() != "Previous" {
		t.Errorf("apps: page %d next %v prev %q %q", q.Page, q.HasNext(), q.PrevURL(), q.PrevLabel())
	}
}

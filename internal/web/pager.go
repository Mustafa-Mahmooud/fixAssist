package web

import (
	"net/http"
	"net/url"
	"strconv"
)

// Page sizes of the dashboard's paged lists.
const (
	RunsPerPage        = 10 // Setup: recent auto-fixes
	AlertsPerPage      = 25 // Alerts: sent alerts
	InsightsPerPage    = 10 // Insights: growth insights (cards)
	QuarantinedPerPage = 25 // Admin: quarantined fixes
	AdminAppsPerPage   = 50 // Admin: ingest by app
	AccountsPerPage    = 25 // Admin: accounts
)

// pager is one page of a list: "11–20 of 43" with links to the pages either
// side. Each list has its own query parameter, so two lists on one page page
// separately, and the page's other parameters (a tab, a filter) are kept.
type pager struct {
	Page, Pages, Total, size int

	param, anchor string
	query         url.Values
	chrono        bool // newest first: "Newer"/"Older" rather than "Previous"/"Next"
}

// newPager clamps the requested page (1 is the first) to the pages there are.
func newPager(page, total, size int) pager {
	pages := max(1, (total+size-1)/size)
	return pager{Page: min(max(page, 1), pages), Pages: pages, Total: total, size: size}
}

// pageOf is the pager for the list whose page is in r's query parameter
// param. anchor is the id the links jump back to ("" for the top).
func pageOf(r *http.Request, param string, total, size int, anchor string, chrono bool) pager {
	n, _ := strconv.Atoi(r.URL.Query().Get(param))
	p := newPager(n, total, size)
	p.param, p.anchor, p.query, p.chrono = param, anchor, r.URL.Query(), chrono
	return p
}

func (p pager) Offset() int     { return (p.Page - 1) * p.size }
func (p pager) Limit() int32    { return int32(p.size) }
func (p pager) Off() int32      { return int32(p.Offset()) }
func (p pager) First() int      { return min(p.Offset()+1, p.Total) }
func (p pager) Last() int       { return min(p.Offset()+p.size, p.Total) }
func (p pager) HasPrev() bool   { return p.Page > 1 }
func (p pager) HasNext() bool   { return p.Page < p.Pages }
func (p pager) Multiple() bool  { return p.Pages > 1 }
func (p pager) PrevURL() string { return p.url(p.Page - 1) }
func (p pager) NextURL() string { return p.url(p.Page + 1) }

func (p pager) PrevLabel() string {
	if p.chrono {
		return "Newer"
	}
	return "Previous"
}

func (p pager) NextLabel() string {
	if p.chrono {
		return "Older"
	}
	return "Next"
}

// url is page n of this list, keeping the request's other parameters.
func (p pager) url(n int) string {
	q := url.Values{}
	for k, v := range p.query {
		q[k] = v
	}
	if n <= 1 {
		q.Del(p.param)
	} else {
		q.Set(p.param, strconv.Itoa(n))
	}
	u := "?" + q.Encode()
	if p.anchor != "" {
		u += "#" + p.anchor
	}
	return u
}

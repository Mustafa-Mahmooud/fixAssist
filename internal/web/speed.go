package web

import (
	"fmt"
	"net/http"

	"github.com/yourname/fixassist/internal/analytics"
)

// speedPage is the Speed screen: Core Web Vitals (p75) for the whole app
// and each page, measured in visitors' browsers, for all devices or one.
func (a *App) speedPage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok {
		return
	}
	rng := rangeFrom(r)
	device := r.URL.Query().Get("device")
	if device != "mobile" && device != "desktop" {
		device = ""
	}
	site, pages, err := analytics.Speed(r.Context(), a.Pool, p.ID, rng.From.Time, rng.To.Time, device)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.render(w, 200, "speed", page{Title: "Speed", Session: sessionFrom(r), Data: map[string]any{
		"Project": p, "Range": rng, "Device": device, "Site": site, "Pages": pages,
		"Vitals": analytics.Vitals[:3], "More": analytics.Vitals[3:], "MinSamples": analytics.MinVitalSamples,
	}})
}

// vitalText is a p75 as founders read it: "1.6 s", "450 ms", "0.08", or
// "—" when there are too few measurements.
func vitalText(metric string, s analytics.VitalStat) string {
	if s.Rating == "" {
		return "—"
	}
	switch {
	case metric == "CLS":
		return fmt.Sprintf("%.2f", s.P75)
	case s.P75 >= 1000:
		return fmt.Sprintf("%.1f s", s.P75/1000)
	}
	return fmt.Sprintf("%.0f ms", s.P75)
}

// vitalClass is a rating's badge style.
func vitalClass(rating string) string {
	switch rating {
	case "good":
		return "fixed"
	case "needs-improvement":
		return "explained"
	case "poor":
		return "open"
	}
	return "ignored"
}

// vitalWord is a rating in words.
func vitalWord(rating string) string {
	switch rating {
	case "good":
		return "Good"
	case "needs-improvement":
		return "Needs work"
	case "poor":
		return "Poor"
	}
	return "Too few visits"
}

package web

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yourname/fixassist/internal/store/db"
)

// Usage analytics screens (FDR Phase 6). They read rollups only.

type dayRange struct {
	Days     int
	From, To pgtype.Date
}

func rangeFrom(r *http.Request) dayRange {
	days := 7
	if r.URL.Query().Get("days") == "30" {
		days = 30
	}
	to := time.Now().UTC().Truncate(24 * time.Hour)
	return dayRange{Days: days, From: pgtype.Date{Time: to.AddDate(0, 0, -(days - 1)), Valid: true}, To: pgtype.Date{Time: to, Valid: true}}
}

func percent(part, whole int64) string {
	if whole <= 0 {
		return "—"
	}
	p := float64(part) * 100 / float64(whole)
	if p > 0 && p < 1 {
		return fmt.Sprintf("%.1f%%", p)
	}
	return fmt.Sprintf("%.0f%%", p)
}

// dayChart draws visitors per day as an inline SVG bar chart with labels.
func dayChart(rng dayRange, rows []db.OverviewDailyRow) template.HTML {
	byDay := map[string]int32{}
	maxV := int32(0)
	for _, r := range rows {
		byDay[r.Day.Time.Format("2006-01-02")] = r.Visitors
		maxV = max(maxV, r.Visitors)
	}
	const w, h, pad = 700.0, 140.0, 18.0
	n := rng.Days
	bw := w / float64(n)
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %.0f %.0f" role="img" aria-label="Visitors per day">`, w, h+pad)
	for i := 0; i < n; i++ {
		day := rng.From.Time.AddDate(0, 0, i)
		v := byDay[day.Format("2006-01-02")]
		bh := 0.0
		if maxV > 0 {
			bh = float64(v) / float64(maxV) * (h - 14)
		}
		x := float64(i) * bw
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f"><title>%s: %d visitors</title></rect>`,
			x+bw*0.15, h-bh, bw*0.7, bh, day.Format("Mon 2 Jan"), v)
		if n <= 7 || i%5 == 0 || i == n-1 {
			fmt.Fprintf(&b, `<text x="%.1f" y="%.0f" text-anchor="middle">%s</text>`, x+bw/2, h+pad-4, day.Format("2 Jan"))
		}
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

func (a *App) overviewPage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok {
		return
	}
	rng := rangeFrom(r)
	q := db.New(a.Pool)
	ctx := r.Context()
	tot, err := q.OverviewTotals(ctx, db.OverviewTotalsParams{ProjectID: p.ID, Day: rng.From, Day_2: rng.To})
	if err != nil {
		a.fail(w, err)
		return
	}
	daily, err := q.OverviewDaily(ctx, db.OverviewDailyParams{ProjectID: p.ID, Day: rng.From, Day_2: rng.To})
	if err != nil {
		a.fail(w, err)
		return
	}
	sources, err := q.TopSources(ctx, db.TopSourcesParams{ProjectID: p.ID, Day: rng.From, Day_2: rng.To})
	if err != nil {
		a.fail(w, err)
		return
	}
	devices, err := q.TopDimension(ctx, db.TopDimensionParams{ProjectID: p.ID, Dimension: "device", Day: rng.From, Day_2: rng.To})
	if err != nil {
		a.fail(w, err)
		return
	}
	countries, err := q.TopDimension(ctx, db.TopDimensionParams{ProjectID: p.ID, Dimension: "country", Day: rng.From, Day_2: rng.To})
	if err != nil {
		a.fail(w, err)
		return
	}
	a.render(w, 200, "overview", page{Title: "Overview", Session: sessionFrom(r), Data: map[string]any{
		"Project": p, "Range": rng, "Totals": tot, "Conversion": percent(tot.Signups, tot.Visitors),
		"Chart": dayChart(rng, daily), "Sources": sources, "Devices": devices, "Countries": countries,
	}})
}

type pageRow struct {
	db.TopPagesRow
	ExitRate, MobileExitRate string
}

func (a *App) pagesPage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok {
		return
	}
	rng := rangeFrom(r)
	q := db.New(a.Pool)
	ctx := r.Context()
	pages, err := q.TopPages(ctx, db.TopPagesParams{ProjectID: p.ID, Day: rng.From, Day_2: rng.To})
	if err != nil {
		a.fail(w, err)
		return
	}
	rows := make([]pageRow, 0, len(pages))
	for _, pg := range pages {
		rows = append(rows, pageRow{TopPagesRow: pg, ExitRate: percent(pg.Exits, pg.Sessions), MobileExitRate: percent(pg.MobileExits, pg.MobileSessions)})
	}
	clicks, err := q.TopClicks(ctx, db.TopClicksParams{ProjectID: p.ID, Day: rng.From, Day_2: rng.To})
	if err != nil {
		a.fail(w, err)
		return
	}
	events, err := q.TopEvents(ctx, db.TopEventsParams{ProjectID: p.ID, Day: rng.From, Day_2: rng.To})
	if err != nil {
		a.fail(w, err)
		return
	}
	a.render(w, 200, "pages", page{Title: "Pages", Session: sessionFrom(r), Data: map[string]any{
		"Project": p, "Range": rng, "Pages": rows, "Clicks": clicks, "Events": events,
	}})
}

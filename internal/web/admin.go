package web

import (
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/yourname/fixassist/internal/store/db"
)

// isOwner: listed in STAFF_EMAILS, so always admin and never demotable.
func (a *App) isOwner(email string) bool {
	return slices.Contains(a.Staff, strings.ToLower(email))
}

// Staff-only area (FDR): ingest rates, 429s, queue depth and lag, AI cost
// and prompt outcomes, raw issue data. Staff = role 'admin' or STAFF_EMAILS;
// everyone else gets a 404 so the area's existence isn't revealed.
func (a *App) staffOnly(next http.HandlerFunc) http.Handler {
	return a.signedIn(func(w http.ResponseWriter, r *http.Request) {
		if !sessionFrom(r).Staff {
			http.NotFound(w, r)
			return
		}
		next(w, r)
	})
}

type codeCount struct {
	Code  string
	Count int64
}

type outcomeRow struct {
	db.AdminSuggestionStatsRow
	Rate string
}

func (a *App) adminPage(w http.ResponseWriter, r *http.Request) {
	q := db.New(a.Pool)
	projects, err := q.AdminIngestByProject(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	queues, err := q.AdminQueues(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	lags, err := q.AdminOldestAvailable(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	stats, err := q.AdminSuggestionStats(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	var outcomes []outcomeRow
	var worked, failed int64
	for _, s := range stats {
		row := outcomeRow{AdminSuggestionStatsRow: s, Rate: "—"}
		if s.Worked+s.Failed > 0 {
			row.Rate = strconv.FormatInt(s.Worked*100/(s.Worked+s.Failed), 10) + "%"
		}
		worked += s.Worked
		failed += s.Failed
		outcomes = append(outcomes, row)
	}
	overall := "no answers yet"
	if worked+failed > 0 {
		overall = strconv.FormatInt(worked*100/(worked+failed), 10) + "% worked (" + strconv.FormatInt(worked, 10) + " of " + strconv.FormatInt(worked+failed, 10) + ")"
	}
	var codes []codeCount
	if a.Metrics != nil {
		for c, n := range a.Metrics.RequestCounts("ingest.") {
			codes = append(codes, codeCount{Code: c, Count: int64(n)})
		}
		sort.Slice(codes, func(i, j int) bool { return codes[i].Code < codes[j].Code })
	}
	quarantined, err := q.AdminQuarantined(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	a.render(w, 200, "admin", page{Title: "Admin", Session: sessionFrom(r), Data: map[string]any{
		"Projects": projects, "Queues": queues, "Lags": lags, "Outcomes": outcomes, "Overall": overall, "Codes": codes,
		"Quarantined": quarantined,
	}})
}

func (a *App) adminIssue(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	raw, err := db.New(a.Pool).AdminIssueRaw(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(prettyJSON([]byte(raw))))
}

// adminUnquarantine resets a quarantined pattern after staff rewrote its
// catalog entry: its track record starts again from zero.
func (a *App) adminUnquarantine(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := db.New(a.Pool).AdminUnquarantine(r.Context(), id); err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

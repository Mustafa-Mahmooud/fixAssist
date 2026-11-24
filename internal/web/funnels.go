package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yourname/fixassist/internal/analytics"
	"github.com/yourname/fixassist/internal/store/db"
)

// Goals and funnels (FDR Phase 6). The differentiator: every funnel step
// shows the open issues on that step's page, with the fix one click away.

type overlayIssue struct {
	ID        int64
	Title     string
	UserCount int64
}

type funnelStep struct {
	Label             string
	Sessions          int64
	Mobile, Desktop   int64
	FromPrev, DropPct string
	BarPct            int
	Issues            []overlayIssue
}

type funnelView struct {
	ID      int64
	Name    string
	Steps   []funnelStep
	Overall string
}

type goalView struct {
	db.ListGoalsRow
	Conversions int64
	Rate        string
}

func stepLabel(s analytics.Step) string {
	switch s.Kind {
	case "page":
		return "Visited " + s.Match
	case "click":
		return "Clicked “" + s.Match + "”"
	case "event":
		return "Event " + s.Match
	case "signup":
		return "Signed up"
	}
	return s.Match
}

// overlayFor picks the open issues that belong to a funnel step.
func overlayFor(s analytics.Step, issues []db.OpenIssuesForOverlayRow) []overlayIssue {
	var out []overlayIssue
	for _, is := range issues {
		match := false
		switch s.Kind {
		case "page":
			match = is.Route.String == s.Match
		case "signup":
			match = strings.Contains(is.Signature, "/auth/v1/signup") || strings.Contains(is.Signature, "accounts:signUp")
		}
		if match && len(out) < 3 {
			out = append(out, overlayIssue{ID: is.ID, Title: is.Title, UserCount: is.UserCount})
		}
	}
	return out
}

func (a *App) funnelsPage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok {
		return
	}
	rng := rangeFrom(r)
	q := db.New(a.Pool)
	ctx := r.Context()
	funnels, err := q.ListFunnels(ctx, p.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	stats, err := q.FunnelStats(ctx, db.FunnelStatsParams{ProjectID: p.ID, Day: rng.From, Day_2: rng.To})
	if err != nil {
		a.fail(w, err)
		return
	}
	issues, err := q.OpenIssuesForOverlay(ctx, p.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	type key struct {
		f int64
		s int32
	}
	total, mobile, desktop := map[key]int64{}, map[key]int64{}, map[key]int64{}
	for _, s := range stats {
		k := key{s.FunnelID, s.Step}
		total[k] += s.Sessions
		switch s.Device {
		case "mobile":
			mobile[k] += s.Sessions
		case "desktop":
			desktop[k] += s.Sessions
		}
	}
	var views []funnelView
	for _, f := range funnels {
		steps, err := analytics.ParseSteps(f.Steps)
		if err != nil {
			continue
		}
		v := funnelView{ID: f.ID, Name: f.Name}
		first := total[key{f.ID, 0}]
		for i, s := range steps {
			k := key{f.ID, int32(i)}
			fs := funnelStep{Label: stepLabel(s), Sessions: total[k], Mobile: mobile[k], Desktop: desktop[k], Issues: overlayFor(s, issues)}
			if first > 0 {
				fs.BarPct = int(total[k] * 100 / first)
			}
			if i > 0 {
				prev := total[key{f.ID, int32(i - 1)}]
				fs.FromPrev = percent(total[k], prev)
				if prev > 0 {
					fs.DropPct = percent(prev-total[k], prev)
				}
			}
			v.Steps = append(v.Steps, fs)
		}
		if len(steps) > 1 {
			v.Overall = percent(total[key{f.ID, int32(len(steps) - 1)}], first)
		}
		views = append(views, v)
	}

	goals, err := q.ListGoals(ctx, p.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	gstats, err := q.GoalStats(ctx, db.GoalStatsParams{ProjectID: p.ID, Day: rng.From, Day_2: rng.To})
	if err != nil {
		a.fail(w, err)
		return
	}
	tot, err := q.OverviewTotals(ctx, db.OverviewTotalsParams{ProjectID: p.ID, Day: rng.From, Day_2: rng.To})
	if err != nil {
		a.fail(w, err)
		return
	}
	conv := map[int64]int64{}
	for _, g := range gstats {
		conv[g.GoalID] = g.Conversions
	}
	var gv []goalView
	for _, g := range goals {
		gv = append(gv, goalView{ListGoalsRow: g, Conversions: conv[g.ID], Rate: percent(conv[g.ID], tot.Visitors)})
	}

	// Suggested first funnel: the two most-viewed pages, then sign-up.
	var suggestion []analytics.Step
	if len(funnels) == 0 {
		if top, err := q.TopPages(ctx, db.TopPagesParams{ProjectID: p.ID, Day: rng.From, Day_2: rng.To}); err == nil {
			for _, pg := range top {
				if len(suggestion) < 2 {
					suggestion = append(suggestion, analytics.Step{Kind: "page", Match: pg.Route})
				}
			}
		}
		suggestion = append(suggestion, analytics.Step{Kind: "signup"})
	}
	var sugLabels []string
	for _, s := range suggestion {
		sugLabels = append(sugLabels, stepLabel(s))
	}
	sugJSON, _ := json.Marshal(suggestion)

	a.render(w, 200, "funnels", page{Title: "Funnels", Session: sessionFrom(r), Data: map[string]any{
		"Project": p, "Range": rng, "Funnels": views, "Goals": gv,
		"Suggestion": strings.Join(sugLabels, " → "), "SuggestionJSON": string(sugJSON),
	}})
}

var stepKinds = map[string]bool{"page": true, "click": true, "event": true, "signup": true}

func (a *App) createFunnel(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	var steps []analytics.Step
	if raw := r.PostFormValue("steps_json"); raw != "" { // the suggested funnel
		if s, err := analytics.ParseSteps([]byte(raw)); err == nil {
			steps = s
		}
	} else {
		kinds, matches := r.PostForm["step_kind"], r.PostForm["step_match"]
		for i := range kinds {
			if i >= len(matches) || i >= 8 {
				break
			}
			m := strings.TrimSpace(matches[i])
			if !stepKinds[kinds[i]] || (m == "" && kinds[i] != "signup") {
				continue
			}
			steps = append(steps, analytics.Step{Kind: kinds[i], Match: m})
		}
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		name = "Funnel"
	}
	if len(steps) < 2 {
		http.Error(w, "a funnel needs at least 2 steps", http.StatusBadRequest)
		return
	}
	raw, _ := json.Marshal(steps)
	if _, err := db.New(a.Pool).InsertFunnel(r.Context(), db.InsertFunnelParams{ProjectID: p.ID, Name: name, Steps: raw}); err != nil {
		a.fail(w, err)
		return
	}
	a.backfillAndBack(w, r, p.ID, "funnels")
}

func (a *App) createGoal(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok {
		return
	}
	kind, match, name := r.PostFormValue("kind"), strings.TrimSpace(r.PostFormValue("match")), strings.TrimSpace(r.PostFormValue("name"))
	if kind != "page" && kind != "click" && kind != "event" || match == "" || name == "" {
		http.Error(w, "goal needs a name, a type and what to match", http.StatusBadRequest)
		return
	}
	if _, err := db.New(a.Pool).InsertGoal(r.Context(), db.InsertGoalParams{ProjectID: p.ID, Name: name, Kind: kind, Match: match, IsLead: r.PostFormValue("is_lead") == "on"}); err != nil {
		a.fail(w, err)
		return
	}
	a.backfillAndBack(w, r, p.ID, "funnels")
}

func (a *App) deleteFunnelOrGoal(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("item"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	q := db.New(a.Pool)
	if r.PathValue("what") == "goals" {
		_, err = q.DeleteGoal(r.Context(), db.DeleteGoalParams{ID: id, ProjectID: p.ID})
	} else {
		_, err = q.DeleteFunnel(r.Context(), db.DeleteFunnelParams{ID: id, ProjectID: p.ID})
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/projects/%d/funnels", p.ID), http.StatusSeeOther)
}

// backfillAndBack recomputes the last 30 days so the new funnel or goal has
// history immediately, then returns to the funnels page.
func (a *App) backfillAndBack(w http.ResponseWriter, r *http.Request, projectID int64, to string) {
	if err := analytics.Backfill(r.Context(), a.Pool, time.Now(), 30); err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/projects/%d/%s", projectID, to), http.StatusSeeOther)
}

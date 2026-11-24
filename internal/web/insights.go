package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yourname/fixassist/internal/analytics"
	"github.com/yourname/fixassist/internal/store/db"
)

type insightView struct {
	db.ListInsightsRow
	Prompt string
	Status string // what the before/after check is doing
	Result string // "went from X to Y"
}

func formatMetric(m analytics.Metric, v float64) string {
	if m.Type == "page_load" {
		return fmt.Sprintf("%.1f s", v/1000)
	}
	return fmt.Sprintf("%.0f%%", v*100)
}

func (a *App) insightsPage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok {
		return
	}
	list, err := db.New(a.Pool).ListInsights(r.Context(), p.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	var views []insightView
	for _, in := range list {
		var m analytics.Metric
		_ = json.Unmarshal(in.Metric, &m)
		v := insightView{ListInsightsRow: in,
			Prompt: "Please make this change in my app.\n\n" + in.FixPrompt + "\n\nOnly make this one change.\n\nTo check it worked: " + in.HowToCheck}
		switch {
		case in.HasAfter:
			v.Result = fmt.Sprintf("Before: %s. After your next release: %s.", formatMetric(m, in.Baseline), formatMetric(m, in.AfterValue))
		case in.Outcome.Valid:
			v.Status = "You marked this."
		default:
			v.Status = fmt.Sprintf("After your next release we compare %s over %d days (now: %s).", metricName(m), 7, formatMetric(m, in.Baseline))
		}
		views = append(views, v)
	}
	a.render(w, 200, "insights", page{Title: "Insights", Session: sessionFrom(r), Data: map[string]any{"Project": p, "Insights": views}})
}

func metricName(m analytics.Metric) string {
	switch m.Type {
	case "funnel_step":
		return "how many people continue at this step"
	case "page_exit":
		return "how many phone visitors leave this page"
	case "page_load":
		return "this page's load time"
	case "signup_rate":
		return "your sign-up rate"
	case "source_rate":
		return "this source's sign-up rate"
	}
	return "the number"
}

func (a *App) generateInsights(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok {
		return
	}
	if err := analytics.Run(r.Context(), a.Pool, time.Now()); err != nil { // fresh rollups first
		a.fail(w, err)
		return
	}
	if _, err := analytics.Generate(r.Context(), a.Pool, p.ID, time.Now()); err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/projects/%d/insights", p.ID), http.StatusSeeOther)
}

func (a *App) insightOutcome(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("insight"), 10, 64)
	outcome := r.PostFormValue("outcome")
	if err != nil || (outcome != "worked" && outcome != "no_change" && outcome != "worse") {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if _, err := db.New(a.Pool).SetInsightOutcome(r.Context(), db.SetInsightOutcomeParams{ID: id, ProjectID: p.ID, Outcome: pgText(outcome)}); err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/projects/%d/insights", p.ID), http.StatusSeeOther)
}

func pgText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

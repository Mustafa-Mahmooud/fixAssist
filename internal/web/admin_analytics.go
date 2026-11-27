package web

import (
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yourname/fixassist/internal/store/db"
)

// Admin analytics: platform growth (/admin/growth), every account
// (/admin/accounts) and one account in depth (/admin/accounts/{id}).

// stages is the activation path a new account walks, in order.
var stages = []struct{ Label, Stuck string }{
	{"Signed up", "Signed up but never added an app."},
	{"Added an app", "Added an app but never installed the script: no visits or errors yet."},
	{"Connected", "Script works, but no error caught yet. Fine if the app is healthy."},
	{"Issue caught", "Errors caught, but no fix prompt written yet."},
	{"Fix prompt sent", "Got fix prompts, but none confirmed as working yet."},
	{"Fix worked", "At least one fix confirmed as working."},
}

// stageOf is how far an account got: the index into stages.
func stageOf(a db.AdminAccountsRow) int {
	for i, done := range []bool{a.HasApp, a.Connected, a.Caught, a.Prompted, a.Fixed} {
		if !done {
			return i
		}
	}
	return len(stages) - 1
}

type accountView struct {
	db.AdminAccountsRow
	Stage, StageOf int
	StageLabel     string
	StageHint      string
	Members        []memberView
	Projects       []db.AdminAccountProjectsRow
	Visitors       int64 // all its apps, last 7 days
	Open           int64 // open issues across its apps
}

// Pips is one dot per activation stage, filled up to the one reached.
func (v accountView) Pips() []bool {
	p := make([]bool, v.StageOf)
	for i := 0; i < v.Stage; i++ {
		p[i] = true
	}
	return p
}

type memberView struct {
	db.AdminAccountMembersRow
	Owner bool // in STAFF_EMAILS: always admin, no buttons
	Self  bool // you: no buttons, so nobody demotes themselves by accident
}

// loadAccounts joins accounts, their people and their apps.
func (a *App) loadAccounts(r *http.Request) ([]accountView, int, error) {
	q := db.New(a.Pool)
	accounts, err := q.AdminAccounts(r.Context())
	if err != nil {
		return nil, 0, err
	}
	projects, err := q.AdminAccountProjects(r.Context())
	if err != nil {
		return nil, 0, err
	}
	members, err := q.AdminAccountMembers(r.Context())
	if err != nil {
		return nil, 0, err
	}
	appsOf := map[int64][]db.AdminAccountProjectsRow{}
	for _, p := range projects {
		appsOf[p.OrgID] = append(appsOf[p.OrgID], p)
	}
	me := sessionFrom(r).UserID
	membersOf := map[int64][]memberView{}
	for _, m := range members {
		membersOf[m.OrgID] = append(membersOf[m.OrgID], memberView{AdminAccountMembersRow: m, Owner: a.isOwner(m.Email), Self: m.ID == me})
	}
	list := make([]accountView, 0, len(accounts))
	for _, acc := range accounts {
		s := stageOf(acc)
		v := accountView{AdminAccountsRow: acc, Stage: s + 1, StageOf: len(stages), StageLabel: stages[s].Label, StageHint: stages[s].Stuck,
			Members: membersOf[acc.ID], Projects: appsOf[acc.ID]}
		for _, p := range v.Projects {
			v.Visitors += p.Visitors7d
			v.Open += p.OpenIssues
		}
		list = append(list, v)
	}
	return list, len(projects), nil
}

// adminAccounts lists every account with its people and apps. Each app links
// into the normal project pages, where admins have full access.
func (a *App) adminAccounts(w http.ResponseWriter, r *http.Request) {
	list, apps, err := a.loadAccounts(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	active := 0
	for _, v := range list {
		if v.Visitors > 0 {
			active++
		}
	}
	// The stats count every account; the cards below come a page at a time.
	pg := pageOf(r, "page", len(list), AccountsPerPage, "", true)
	a.render(w, 200, "admin_accounts", page{Title: "Accounts", Session: sessionFrom(r), Data: map[string]any{
		"Accounts": list[pg.Offset():pg.Last()], "Total": len(list), "Apps": apps, "Active": active, "Pager": pg,
	}})
}

// adminAccount is one account in depth.
func (a *App) adminAccount(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	q := db.New(a.Pool)
	if _, err := q.AdminGetAccount(r.Context(), id); errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		a.fail(w, err)
		return
	}
	list, _, err := a.loadAccounts(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	var acc *accountView
	for i := range list {
		if list[i].ID == id {
			acc = &list[i]
		}
	}
	if acc == nil { // beyond the list's limit: very old account
		http.NotFound(w, r)
		return
	}
	daily, err := q.AdminAccountDaily(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	issues, err := q.AdminTopIssues(r.Context(), pgtype.Int8{Int64: id, Valid: true})
	if err != nil {
		a.fail(w, err)
		return
	}
	var days []pgtype.Date
	var visitors, errs, active []float64
	for _, d := range daily {
		days = append(days, d.Day)
		visitors = append(visitors, float64(d.Visitors))
		errs = append(errs, float64(d.Errors))
		active = append(active, float64(d.ActiveUsers))
	}
	var v30, su30, fixed int64
	for _, p := range acc.Projects {
		v30 += p.Visitors30d
		su30 += p.Signups30d
		fixed += p.FixedIssues
	}
	a.render(w, 200, "admin_account", page{Title: "Account", Session: sessionFrom(r), Data: map[string]any{
		"Account": acc, "Issues": issues, "Stages": stages,
		"Visitors30d": v30, "Signups30d": su30, "Fixed": fixed,
		"VisitorsChart": barChart("Their visitors per day", days, visitors, countFmt("visitors")),
		"ErrorsChart":   barChart("Errors caught per day", days, errs, countFmt("errors")),
		"ActiveChart":   barChart("Their team using the dashboard per day", days, active, countFmt("people")),
	}})
}

type activationStep struct {
	Label, Hint       string
	Count             int64
	OfStart, FromPrev string
	Width             int // bar width, % of the first step
}

type costRow struct {
	ID   int64
	Name string
	Cost float64
}

// adminGrowth is the platform at a glance: growth, activation, product
// health and AI spend.
func (a *App) adminGrowth(w http.ResponseWriter, r *http.Request) {
	q := db.New(a.Pool)
	ctx := r.Context()
	t, err := q.AdminPlatformTotals(ctx)
	if err != nil {
		a.fail(w, err)
		return
	}
	series, err := q.AdminDailySeries(ctx)
	if err != nil {
		a.fail(w, err)
		return
	}
	list, _, err := a.loadAccounts(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	issues, err := q.AdminTopIssues(ctx, pgtype.Int8{})
	if err != nil {
		a.fail(w, err)
		return
	}
	kinds, err := q.AdminIssueKinds(ctx)
	if err != nil {
		a.fail(w, err)
		return
	}
	builders, err := q.AdminAppsByBuilder(ctx)
	if err != nil {
		a.fail(w, err)
		return
	}
	models, err := q.AdminAIByModel(ctx)
	if err != nil {
		a.fail(w, err)
		return
	}

	setupRows, err := q.AdminSetupTimes(ctx)
	if err != nil {
		a.fail(w, err)
		return
	}
	setupSteps, nudges := setupTimes(setupRows)

	// Activation funnel: how many accounts reached each stage.
	reached := make([]int64, len(stages))
	for _, v := range list {
		for i := 0; i < v.Stage; i++ {
			reached[i]++
		}
	}
	var funnel []activationStep
	for i, s := range stages {
		f := activationStep{Label: s.Label, Count: reached[i], OfStart: percent(reached[i], reached[0]), Width: 0}
		if reached[0] > 0 {
			f.Width = int(reached[i] * 100 / reached[0])
		}
		if i > 0 {
			f.FromPrev = percent(reached[i], reached[i-1])
			f.Hint = "lost " + strconv.FormatInt(reached[i-1]-reached[i], 10) + " here"
		}
		funnel = append(funnel, f)
	}

	recent := list
	if len(recent) > 15 {
		recent = recent[:15] // newest first
	}
	costs := make([]costRow, 0, len(list))
	for _, v := range list {
		if v.AiCostUsd > 0 {
			costs = append(costs, costRow{ID: v.ID, Name: v.Name, Cost: v.AiCostUsd})
		}
	}
	sort.Slice(costs, func(i, j int) bool { return costs[i].Cost > costs[j].Cost })
	if len(costs) > 10 {
		costs = costs[:10]
	}

	var days []pgtype.Date
	cols := map[string][]float64{}
	for _, d := range series {
		days = append(days, d.Day)
		cols["signups"] = append(cols["signups"], float64(d.Signups))
		cols["active"] = append(cols["active"], float64(d.ActiveUsers))
		cols["visitors"] = append(cols["visitors"], float64(d.Visitors))
		cols["errors"] = append(cols["errors"], float64(d.Errors))
		cols["issues"] = append(cols["issues"], float64(d.NewIssues))
		cols["prompts"] = append(cols["prompts"], float64(d.Prompts))
		cols["cost"] = append(cols["cost"], d.AiCost)
	}
	charts := []struct {
		Title string
		SVG   template.HTML
	}{
		{"New sign-ups", barChart("New sign-ups per day", days, cols["signups"], countFmt("sign-ups"))},
		{"People using the dashboard", barChart("Active users per day", days, cols["active"], countFmt("people"))},
		{"Visitors to customers' apps", barChart("Visitors to customers' apps per day", days, cols["visitors"], countFmt("visitors"))},
		{"Errors caught", barChart("Errors caught per day", days, cols["errors"], countFmt("errors"))},
		{"New issues", barChart("New issues per day", days, cols["issues"], countFmt("issues"))},
		{"Fix prompts written", barChart("Fix prompts per day", days, cols["prompts"], countFmt("prompts"))},
		{"AI cost", barChart("AI cost per day", days, cols["cost"], func(v float64) string { return fmt.Sprintf("$%.4f", v) })},
	}

	worked := percent(t.Worked30d, t.Worked30d+t.Failed30d)
	costPerApp := "—"
	if t.LiveApps7d > 0 {
		costPerApp = fmt.Sprintf("$%.4f", t.AiCost30d/float64(t.LiveApps7d))
	}
	a.render(w, 200, "admin_growth", page{Title: "Growth", Session: sessionFrom(r), Data: map[string]any{
		"T": t, "Worked": worked, "CostPerApp": costPerApp,
		"AccountsTrend": trend(float64(t.Accounts30d), float64(t.AccountsPrev)),
		"VisitorsTrend": trend(float64(t.Visitors30d), float64(t.VisitorsPrev)),
		"IssuesTrend":   trend(float64(t.Issues30d), float64(t.IssuesPrev)),
		"CostTrend":     trend(t.AiCost30d, t.AiCostPrev),
		"Stickiness":    percent(t.ActiveUsers7d, t.ActiveUsers30d),
		"Funnel":        funnel, "Recent": recent, "Charts": charts, "SetupSteps": setupSteps, "Nudges": nudges, "SetupApps": len(setupRows),
		"Issues": issues, "Kinds": kinds, "Builders": builders, "Models": models, "Costs": costs,
	}})
}

// trend compares this period with the one before, e.g. "↑ 40% vs previous 30 days".
func trend(cur, prev float64) string {
	switch {
	case prev == 0 && cur == 0:
		return "no change vs previous 30 days"
	case prev == 0:
		return "new: none in the previous 30 days"
	}
	ch := (cur - prev) / prev * 100
	arrow := "↑"
	if ch < 0 {
		arrow = "↓"
	}
	return fmt.Sprintf("%s %.0f%% vs previous 30 days", arrow, math.Abs(ch))
}

func countFmt(unit string) func(float64) string {
	return func(v float64) string { return strconv.FormatFloat(v, 'f', 0, 64) + " " + unit }
}

// barChart draws one value per day as an inline SVG bar chart; hovering a
// bar shows the day and value.
func barChart(label string, days []pgtype.Date, vals []float64, format func(float64) string) template.HTML {
	n := len(vals)
	if n == 0 {
		return ""
	}
	maxV := 0.0
	for _, v := range vals {
		maxV = max(maxV, v)
	}
	const w, h, pad = 700.0, 110.0, 18.0
	bw := w / float64(n)
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">`, w, h+pad, template.HTMLEscapeString(label))
	for i, v := range vals {
		bh := 0.0
		if maxV > 0 {
			bh = v / maxV * (h - 14)
		}
		x := float64(i) * bw
		day := days[i].Time
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f"><title>%s: %s</title></rect>`,
			x+bw*0.15, h-bh, bw*0.7, bh, day.Format("Mon 2 Jan"), template.HTMLEscapeString(format(v)))
		if i%5 == 0 || i == n-1 {
			fmt.Fprintf(&b, `<text x="%.1f" y="%.0f" text-anchor="middle">%s</text>`, x+bw/2, h+pad-4, day.Format("2 Jan"))
		}
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// setUserRole makes a person an admin or a normal user. Owners (STAFF_EMAILS)
// stay admin whatever their role says, and nobody changes their own role.
func (a *App) setUserRole(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	role := r.PostFormValue("role")
	if err != nil || (role != "admin" && role != "user") {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if id == sessionFrom(r).UserID {
		http.Error(w, "you can't change your own role", http.StatusForbidden)
		return
	}
	n, err := db.New(a.Pool).SetUserRole(r.Context(), db.SetUserRoleParams{ID: id, Role: role})
	if err != nil {
		a.fail(w, err)
		return
	}
	if n == 0 {
		http.NotFound(w, r)
		return
	}
	a.Log.Info("user role changed", "user_id", id, "role", role, "by", sessionFrom(r).UserID)
	http.Redirect(w, r, backTo(r, "/admin/accounts"), http.StatusSeeOther)
}

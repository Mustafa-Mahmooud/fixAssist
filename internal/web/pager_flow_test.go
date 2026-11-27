package web_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/yourname/fixassist/internal/auth"
	"github.com/yourname/fixassist/internal/projects"
	"github.com/yourname/fixassist/internal/web"
)

// Long lists come a page at a time: nothing older is cut off, and each page
// says where it is.
func TestPagedLists(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	as := &auth.Service{Pool: f.pool, Sender: f.mail, BaseURL: srv.URL}
	(&web.App{Pool: f.pool, Auth: as, Projects: &projects.Service{Pool: f.pool}, IngestBase: srv.URL, BaseURL: srv.URL,
		Secret: []byte("test-secret-test-secret-test-secret"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Register(mux)
	link, err := as.AdminLink(ctx, "founder@example.com")
	if err != nil {
		t.Fatal(err)
	}
	c := client(t)
	resp, _ := c.PostForm(srv.URL+"/auth/magic", url.Values{"token": {reToken.FindStringSubmatch(link)[1]}})
	body(t, resp)
	var orgID int64
	_ = f.pool.QueryRow(ctx, "select m.org_id from memberships m join users u on u.id = m.user_id where u.email = 'founder@example.com'").Scan(&orgID)
	p, err := (&projects.Service{Pool: f.pool}).Create(ctx, projects.CreateParams{OrgID: orgID, Name: "shop", Agent: "cursor", Backend: "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("%s/projects/%d", srv.URL, p.ID)

	// 60 daily summaries, newest "Summary 1" (sent most recently).
	if _, err := f.pool.Exec(ctx, `insert into alert_log (project_id, kind, day, subject, body, sent_at)
		select $1, 'digest', current_date - g, 'Summary ' || g, 'b', now() - g * interval '1 day' from generate_series(1, 60) g`, p.ID); err != nil {
		t.Fatal(err)
	}
	// 23 insights over 23 weeks, newest "Insight 1".
	if _, err := f.pool.Exec(ctx, `insert into growth_insights (project_id, week, detector, title, evidence, hypothesis, fix_prompt, how_to_check, metric, baseline)
		select $1, current_date - g * 7, 'd', 'Insight ' || g, 'e', 'h', 'f', 'c', '{}', 1 from generate_series(1, 23) g`, p.ID); err != nil {
		t.Fatal(err)
	}

	page := func(path string, want ...string) string {
		t.Helper()
		resp, _ := c.Get(base + path)
		b := body(t, resp)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
		for _, w := range want {
			if !strings.Contains(b, w) {
				t.Errorf("%s doesn't contain %q", path, w)
			}
		}
		return b
	}
	n := web.AlertsPerPage
	b := page("/alerts", fmt.Sprintf("1–%d of 60", n), ">Summary 1<", fmt.Sprintf(">Summary %d<", n), `href="?page=2"`, "Older →")
	if strings.Contains(b, fmt.Sprintf(">Summary %d<", n+1)) {
		t.Error("alerts page 1 shows page 2's alerts")
	}
	page("/alerts?page=2", fmt.Sprintf("%d–%d of 60", n+1, 2*n), fmt.Sprintf(">Summary %d<", n+1), `href="?"`, `href="?page=3"`)
	b = page("/alerts?page=3", fmt.Sprintf("%d–60 of 60", 2*n+1), ">Summary 60<")
	if strings.Contains(b, `rel="next"`) {
		t.Error("the last page links to a next one")
	}
	page("/alerts?page=999", fmt.Sprintf("%d–60 of 60", 2*n+1)) // past the end: the last page

	m := web.InsightsPerPage
	page("/insights", fmt.Sprintf("1–%d of 23", m), "Insight 1<", `href="?page=2"`)
	page("/insights?page=3", fmt.Sprintf("%d–23 of 23", 2*m+1), "Insight 23<")
}

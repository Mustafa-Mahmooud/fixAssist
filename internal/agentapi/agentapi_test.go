package agentapi_test

// Runs only with FIX_TEST_DATABASE_URL (the schema is wiped).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/fixassist/internal/agentapi"
	"github.com/yourname/fixassist/internal/grouping"
	"github.com/yourname/fixassist/internal/ingest"
	"github.com/yourname/fixassist/internal/jobs"
	"github.com/yourname/fixassist/internal/projects"
	"github.com/yourname/fixassist/internal/store"
	"github.com/yourname/fixassist/internal/store/db"
)

type fx struct {
	t      *testing.T
	pool   *pgxpool.Pool
	srv    *httptest.Server
	pid    int64
	token  string
	other  string // a token for another project
	nextID int
	signed bool // the server ships signed daemon builds (Handler.Updates)
}

// backdate moves everything recorded for the issue back by d, as if d had
// passed: the report, the reproduction and the events.
func (f *fx) backdate(issue int64, d time.Duration) {
	f.t.Helper()
	for _, sql := range []string{
		"update fix_attempts set reproduced_at = reproduced_at - make_interval(secs => $2), reported_at = reported_at - make_interval(secs => $2) where issue_id = $1",
		"update events set received_at = received_at - make_interval(secs => $2) where fingerprint = (select fingerprint from issues where id = $1)",
	} {
		if _, err := f.pool.Exec(context.Background(), sql, issue, d.Seconds()); err != nil {
			f.t.Fatal(err)
		}
	}
}

func setup(t *testing.T) *fx {
	url := os.Getenv("FIX_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("FIX_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, "drop schema public cascade; create schema public"); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, pool, log); err != nil {
		t.Fatal(err)
	}
	if err := jobs.RunPartitions(ctx, pool, log, time.Now()); err != nil {
		t.Fatal(err)
	}
	svc := &projects.Service{Pool: pool}
	p, err := svc.Create(ctx, projects.CreateParams{Org: "acme", Name: "Shop", Agent: "cursor", Backend: "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := svc.Create(ctx, projects.CreateParams{Org: "other", Name: "Other", Agent: "lovable", Backend: "supabase"})
	if err != nil {
		t.Fatal(err)
	}
	f := &fx{t: t, pool: pool, pid: p.ID}
	f.token = f.mint(p.ID)
	f.other = f.mint(p2.ID)
	mux := http.NewServeMux()
	(&agentapi.Handler{Pool: pool, Log: log, Updates: func() bool { return f.signed }}).Register(mux)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fx) mint(projectID int64) string {
	tok, hash, prefix, err := agentapi.NewToken()
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := db.New(f.pool).CreateAgentToken(context.Background(), db.CreateAgentTokenParams{ProjectID: projectID, Name: "test", TokenHash: hash, Prefix: prefix}); err != nil {
		f.t.Fatal(err)
	}
	return tok
}

// storeEvent stores a fixture's event for the project and groups it, as
// ingest and the group_event job do. It returns the issue id.
func (f *fx) storeEvent(projectID int64, fixture string) int64 {
	f.t.Helper()
	raw, err := os.ReadFile("../ingest/testdata/server/" + fixture + ".envelope")
	if err != nil {
		f.t.Fatal(err)
	}
	env, err := ingest.ParseEnvelope(raw)
	if err != nil {
		f.t.Fatal(err)
	}
	row, err := ingest.Prepare([]byte("k"), env.Items[0].Payload, time.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	ctx := context.Background()
	var id int64
	var at time.Time
	err = f.pool.QueryRow(ctx, `insert into events (project_id, received_at, kind, fingerprint, release_id, route, payload)
		values ($1, clock_timestamp(), $2, $3, $4, $5, $6) returning id, received_at`,
		projectID, row.Kind, row.Fingerprint, row.ReleaseID, row.Route, row.Payload).Scan(&id, &at)
	if err != nil {
		f.t.Fatal(err)
	}
	res, err := grouping.GroupEvent(ctx, f.pool, id, at, time.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	return res.IssueID
}

type rpcResult struct {
	Result struct {
		ProtocolVersion string `json:"protocolVersion"`
		Instructions    string `json:"instructions"`
		Tools           []struct {
			Name string `json:"name"`
		} `json:"tools"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (f *fx) post(token, body string) *http.Response {
	f.t.Helper()
	req, _ := http.NewRequest("POST", f.srv.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp
}

func (f *fx) rpc(token, method string, params any) rpcResult {
	f.t.Helper()
	f.nextID++
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": f.nextID, "method": method, "params": params})
	resp := f.post(token, string(b))
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		f.t.Fatalf("%s: status %d: %s", method, resp.StatusCode, body)
	}
	var out rpcResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// tool calls a tool and returns its text; wantErr says whether isError is expected.
func (f *fx) tool(name string, args map[string]any, wantErr bool) string {
	f.t.Helper()
	r := f.rpc(f.token, "tools/call", map[string]any{"name": name, "arguments": args})
	if r.Error != nil {
		f.t.Fatalf("%s: rpc error %+v", name, r.Error)
	}
	text := ""
	if len(r.Result.Content) > 0 {
		text = r.Result.Content[0].Text
	}
	if r.Result.IsError != wantErr {
		f.t.Fatalf("%s: isError = %v, want %v: %s", name, r.Result.IsError, wantErr, text)
	}
	return text
}

func mustContain(t *testing.T, what, text string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(text, w) {
			t.Errorf("%s missing %q:\n%s", what, w, text)
		}
	}
}

func TestMCPProtocol(t *testing.T) {
	f := setup(t)
	// No or a wrong token: 401 with a Bearer challenge.
	for _, tok := range []string{"", "fa_nope", "not-a-token"} {
		resp := f.post(tok, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
		resp.Body.Close()
		if resp.StatusCode != 401 || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("token %q: status %d, challenge %q", tok, resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
	}
	init := f.rpc(f.token, "initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "1"}})
	if init.Result.ProtocolVersion != "2025-03-26" {
		t.Errorf("protocolVersion = %q", init.Result.ProtocolVersion)
	}
	mustContain(t, "instructions", init.Result.Instructions, `"Shop"`, "report_fix", "check_fix", "needs_human")
	if f.rpc(f.token, "initialize", map[string]any{"protocolVersion": "1999-01-01"}).Result.ProtocolVersion != "2025-06-18" {
		t.Error("unknown version should get the newest")
	}
	// Notifications get 202 and no body.
	resp := f.post(f.token, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("notification: status %d", resp.StatusCode)
	}
	list := f.rpc(f.token, "tools/list", nil)
	var names []string
	for _, tl := range list.Result.Tools {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "list_issues,get_issue,report_fix,check_fix" {
		t.Errorf("tools = %v", names)
	}
	if r := f.rpc(f.token, "resources/list", nil); r.Error == nil || r.Error.Code != -32601 {
		t.Errorf("unknown method: %+v", r.Error)
	}
	if r := f.rpc(f.token, "tools/call", map[string]any{"name": "rm_rf"}); r.Error == nil || r.Error.Code != -32602 {
		t.Errorf("unknown tool: %+v", r.Error)
	}
	// A batch answers each request.
	resp = f.post(f.token, `[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":2,"method":"tools/list"}]`)
	var batch []json.RawMessage
	_ = json.NewDecoder(resp.Body).Decode(&batch)
	resp.Body.Close()
	if len(batch) != 2 {
		t.Errorf("batch answers = %d, want 2", len(batch))
	}
	// GET has no stream.
	if r, _ := http.Get(f.srv.URL + "/mcp"); r.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /mcp: %d", r.StatusCode)
	}
}

// The whole loop for a server error: explain, report, watch, reproduce,
// came back, next attempt with what was tried, and the attempt limit.
func TestMCPFixLoop(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	issue := f.storeEvent(f.pid, "dotnet_ef_out_of_range")
	foreign := f.storeEvent(mustOther(t, f), "node_unique_chain")

	mustContain(t, "list_issues", f.tool("list_issues", nil, false), fmt.Sprintf("#%d", issue), "Server error", "/Products/Create")
	if strings.Contains(f.tool("list_issues", nil, false), fmt.Sprintf("#%d ", foreign)) {
		t.Error("another project's issue is listed")
	}

	// get_issue explains it on the spot (it was below the threshold).
	text := f.tool("get_issue", map[string]any{"issue_id": issue}, false)
	mustContain(t, "get_issue", text, "Fix prompt (attempt 1 of 3)", "decimal(18,2)", "`Controllers/ProductsController.cs` line 58", "report_fix(issue_id=")
	// Another project's issue looks missing.
	mustContain(t, "foreign get_issue", f.tool("get_issue", map[string]any{"issue_id": foreign}, true), "no such issue")
	mustContain(t, "no id", f.tool("get_issue", map[string]any{}, true), "issue_id is required")

	mustContain(t, "check before report", f.tool("check_fix", map[string]any{"issue_id": issue}, false), "State: not_reported")
	mustContain(t, "report without summary", f.tool("report_fix", map[string]any{"issue_id": issue}, true), "summary is required")

	mustContain(t, "report", f.tool("report_fix", map[string]any{"issue_id": issue, "summary": "Price was decimal(5,2); changed it to decimal(18,2) with a migration.", "files": []string{"Models/Product.cs", "Migrations/20261125_PricePrecision.cs"}}, false), "attempt 1")
	var status string
	_ = f.pool.QueryRow(ctx, "select status from issues where id = $1", issue).Scan(&status)
	if status != "verifying" {
		t.Errorf("status after report = %s", status)
	}

	// No page traffic: it asks the agent to reproduce.
	mustContain(t, "watching", f.tool("check_fix", map[string]any{"issue_id": issue}, false), "State: watching", "reproduced=true")
	mustContain(t, "reproduced", f.tool("check_fix", map[string]any{"issue_id": issue, "reproduced": true}, false), "State: watching", "Reproduction noted")
	f.backdate(issue, 31*time.Second)
	mustContain(t, "after reproduction", f.tool("check_fix", map[string]any{"issue_id": issue}, false), "State: looks_fixed")

	// It happens again: grouping reopens the issue and closes the attempt.
	f.storeEvent(f.pid, "dotnet_ef_out_of_range")
	mustContain(t, "came back", f.tool("check_fix", map[string]any{"issue_id": issue}, false), "State: came_back", "attempt 2")

	// The next prompt is attempt 2 and says what was tried.
	text = f.tool("get_issue", map[string]any{"issue_id": issue}, false)
	mustContain(t, "attempt 2", text, "Fix prompt (attempt 2 of 3)", "A previous fix for this problem was applied",
		"Already tried", "Attempt 1: Price was decimal(5,2)", "Models/Product.cs", "Attempt 1 (agent")

	// Two more failures reach the limit.
	for i := 2; i <= 3; i++ {
		f.tool("report_fix", map[string]any{"issue_id": issue, "summary": fmt.Sprintf("try %d", i)}, false)
		f.storeEvent(f.pid, "dotnet_ef_out_of_range")
	}
	mustContain(t, "needs human", f.tool("check_fix", map[string]any{"issue_id": issue}, false), "State: needs_human", "tell the founder")
	mustContain(t, "refused", f.tool("report_fix", map[string]any{"issue_id": issue, "summary": "again"}, true), "too many times")
	mustContain(t, "get_issue at limit", f.tool("get_issue", map[string]any{"issue_id": issue}, false), "needs a person")

	var attempts, failed int
	_ = f.pool.QueryRow(ctx, "select count(*), count(*) filter (where outcome = 'failed') from fix_attempts where issue_id = $1", issue).Scan(&attempts, &failed)
	if attempts != 3 || failed != 3 {
		t.Errorf("attempts = %d, failed = %d", attempts, failed)
	}
}

func mustOther(t *testing.T, f *fx) int64 {
	var id int64
	if err := f.pool.QueryRow(context.Background(), "select id from projects where name = 'Other'").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// The JSON API: same operations, same project isolation.
func TestRESTAPI(t *testing.T) {
	f := setup(t)
	issue := f.storeEvent(f.pid, "node_numeric_overflow")
	get := func(token, path string) (int, map[string]any) {
		req, _ := http.NewRequest("GET", f.srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	code, m := get(f.token, fmt.Sprintf("/agent/v1/issues/%d", issue))
	if code != 200 || !strings.Contains(fmt.Sprint(m["fix_prompt"]), "numeric field overflow") || m["attempt"] != float64(1) {
		t.Errorf("get: %d %v", code, m)
	}
	if code, _ := get(f.other, fmt.Sprintf("/agent/v1/issues/%d", issue)); code != 404 {
		t.Errorf("other project's token: %d, want 404", code)
	}
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/agent/v1/issues/%d/fix", f.srv.URL, issue), bytes.NewReader([]byte(`{"summary":"Widened price to numeric(12,2)","files":["prisma/schema.prisma"]}`)))
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, _ := http.DefaultClient.Do(req)
	var rep map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&rep)
	resp.Body.Close()
	if resp.StatusCode != 200 || rep["attempt"] != float64(1) {
		t.Errorf("report: %d %v", resp.StatusCode, rep)
	}
	if code, m := get(f.token, fmt.Sprintf("/agent/v1/issues/%d/check", issue)); code != 200 || m["state"] != "watching" {
		t.Errorf("check: %d %v", code, m)
	}
	// A revoked token stops working.
	if _, err := f.pool.Exec(context.Background(), "update agent_tokens set revoked_at = now() where project_id = $1", f.pid); err != nil {
		t.Fatal(err)
	}
	if code, _ := get(f.token, "/agent/v1/issues"); code != 401 {
		t.Errorf("revoked token: %d, want 401", code)
	}
}

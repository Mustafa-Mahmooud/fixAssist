package agentapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// MCP over Streamable HTTP, the subset a tools-only server needs: every
// request is answered with one JSON body (no SSE streams, no sessions),
// notifications get 202. https://modelcontextprotocol.io/specification

// Protocol versions we speak; the newest is offered by default.
var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

const (
	errParse          = -32700
	errInvalidRequest = -32600
	errMethodNotFound = -32601
	errInvalidParams  = -32602
	errInternal       = -32603
)

// instructions is what the agent reads first: the loop.
func instructions(project string) string {
	return "Fix Assistant watches the production app \"" + project + "\" for errors, explains each one with evidence from production, and checks whether a fix really stopped it.\n\n" +
		"Work in this loop, one issue at a time:\n" +
		"1. list_issues to see what needs fixing.\n" +
		"2. get_issue(issue_id) for the evidence and the fix prompt. Make exactly the change it asks for in this codebase.\n" +
		"3. Deploy or publish the change. If you can't deploy, ask the user to, and wait.\n" +
		"4. report_fix(issue_id, summary, files) once it is deployed.\n" +
		"5. check_fix(issue_id) until the state is looks_fixed, fixed, came_back or needs_human. When it asks, repeat the failing action yourself, then call check_fix(issue_id, reproduced=true).\n" +
		"6. On came_back, go back to step 2: get_issue returns the next attempt's prompt with the new evidence and what was already tried.\n" +
		"Stop on needs_human and tell the user. Never claim an issue is fixed unless check_fix says looks_fixed or fixed."
}

type tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

var issueID = map[string]any{"type": "integer", "description": "The issue's id, from list_issues."}

var tools = []tool{
	{
		Name: "list_issues", Title: "List issues",
		Description: "Lists this app's production issues, most important first: open work (new, explained, came back) and fixes being verified. Each line has the id to pass to get_issue.",
		InputSchema: obj(map[string]any{
			"include_fixed": map[string]any{"type": "boolean", "description": "Also list fixed and ignored issues. Default false."},
		}),
		Annotations: map[string]any{"readOnlyHint": true},
	},
	{
		Name: "get_issue", Title: "Get an issue and its fix prompt",
		Description: "Returns one issue: what broke, who is affected, the evidence from production (error chain, stack lines, failing requests), the fix prompt for the current attempt, the fixes already tried, and the next step.",
		InputSchema: obj(map[string]any{"issue_id": issueID}, "issue_id"),
		Annotations: map[string]any{"readOnlyHint": true},
	},
	{
		Name: "report_fix", Title: "Report a deployed fix",
		Description: "Call after the fix is deployed. Records what you changed (used to avoid repeating it if the error comes back) and starts watching for the error on the fixed version.",
		InputSchema: obj(map[string]any{
			"issue_id": issueID,
			"summary":  map[string]any{"type": "string", "description": "What you changed and why, in a sentence or two."},
			"files":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Paths of the files you changed."},
			"release":  map[string]any{"type": "string", "description": "Optional: the version name the fix shipped in, if the app sets one (for example the SENTRY_RELEASE value or a git commit)."},
		}, "issue_id", "summary"),
		Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false},
	},
	{
		Name: "check_fix", Title: "Check whether a fix held",
		Description: "Says whether the latest reported fix held: waiting_for_deploy, watching, came_back, looks_fixed, fixed or needs_human, with what to do next. Set reproduced=true right after you repeat the action that used to fail.",
		InputSchema: obj(map[string]any{
			"issue_id":   issueID,
			"reproduced": map[string]any{"type": "boolean", "description": "True if you just repeated the action that used to fail."},
		}, "issue_id"),
		Annotations: map[string]any{"readOnlyHint": false, "idempotentHint": true},
	},
}

func (h *Handler) mcp(w http.ResponseWriter, r *http.Request) {
	c, err := h.auth(r)
	if err != nil {
		h.unauthorized(w, err)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{errInvalidRequest, "body too large"}})
		return
	}
	body = bytes.TrimSpace(body)
	// A batch (allowed before 2025-06-18) or a single message.
	if len(body) > 0 && body[0] == '[' {
		var batch []rpcRequest
		if err := json.Unmarshal(body, &batch); err != nil || len(batch) == 0 {
			writeJSON(w, http.StatusBadRequest, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{errParse, "invalid JSON-RPC batch"}})
			return
		}
		var out []rpcResponse
		for _, req := range batch {
			if resp, ok := h.handle(r, c, req); ok {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{errParse, "invalid JSON"}})
		return
	}
	resp, ok := h.handle(r, c, req)
	if !ok {
		w.WriteHeader(http.StatusAccepted) // a notification or a response from the client
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handle answers one message; ok is false for messages that get no answer.
func (h *Handler) handle(r *http.Request, c caller, req rpcRequest) (rpcResponse, bool) {
	if len(req.ID) == 0 || string(req.ID) == "null" || req.Method == "" {
		return rpcResponse{}, false
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	fail := func(code int, msg string) (rpcResponse, bool) {
		resp.Error = &rpcError{code, msg}
		return resp, true
	}
	if req.JSONRPC != "2.0" {
		return fail(errInvalidRequest, "jsonrpc must be \"2.0\"")
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := protocolVersions[0]
		if slices.Contains(protocolVersions, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		resp.Result = map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "fix-assistant", "title": "Fix Assistant", "version": "1.0.0"},
			"instructions":    instructions(c.Project),
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": tools}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return fail(errInvalidParams, "params must be {name, arguments}")
		}
		text, err := h.call(r, c, p.Name, p.Arguments)
		var unknown unknownTool
		switch {
		case errors.As(err, &unknown):
			return fail(errInvalidParams, err.Error())
		case err != nil:
			msg := err.Error()
			var ue userError
			if !errors.As(err, &ue) && !errors.Is(err, errNotFound) {
				h.Log.Error("agent tool", "tool", p.Name, "project_id", c.ProjectID, "err", err)
				msg = "Fix Assistant had an internal error. Try again in a moment."
			}
			resp.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": msg}}, "isError": true}
		default:
			resp.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
		}
	default:
		return fail(errMethodNotFound, "unknown method "+req.Method)
	}
	return resp, true
}

type unknownTool struct{ name string }

func (e unknownTool) Error() string { return "unknown tool " + strconv.Quote(e.name) }

// badArgs is a tool error about the arguments.
func badArgs(format string, a ...any) error { return userError{fmt.Sprintf(format, a...)} }

func (h *Handler) call(r *http.Request, c caller, name string, raw json.RawMessage) (string, error) {
	ctx, now := h.ctx(r)
	var args struct {
		IncludeFixed bool     `json:"include_fixed"`
		IssueID      *int64   `json:"issue_id"`
		Summary      string   `json:"summary"`
		Files        []string `json:"files"`
		Release      string   `json:"release"`
		Reproduced   bool     `json:"reproduced"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		dec := json.NewDecoder(bytes.NewReader(raw))
		if err := dec.Decode(&args); err != nil {
			return "", badArgs("invalid arguments: %v", strings.TrimPrefix(err.Error(), "json: "))
		}
	}
	needID := func() (int64, error) {
		if args.IssueID == nil || *args.IssueID <= 0 {
			return 0, badArgs("issue_id is required (an id from list_issues)")
		}
		return *args.IssueID, nil
	}
	switch name {
	case "list_issues":
		text, _, err := h.listIssues(ctx, c, args.IncludeFixed)
		return text, err
	case "get_issue":
		id, err := needID()
		if err != nil {
			return "", err
		}
		text, _, err := h.getIssue(ctx, c, id)
		return text, err
	case "report_fix":
		id, err := needID()
		if err != nil {
			return "", err
		}
		text, _, err := h.reportFix(ctx, c, ReportArgs{IssueID: id, Summary: args.Summary, Files: args.Files, Release: args.Release}, now)
		return text, err
	case "check_fix":
		id, err := needID()
		if err != nil {
			return "", err
		}
		text, _, err := h.checkFix(ctx, c, id, args.Reproduced, now)
		return text, err
	}
	return "", unknownTool{name}
}

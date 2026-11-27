package web

import (
	"encoding/base64"
	"encoding/json"
	"html/template"
	"net/url"
	"strings"
)

// The agent loop: connection setups for coding agents that speak MCP.

// AgentSetup is how to connect one tool.
type AgentSetup struct {
	Name  string
	Where string // where it goes
	Code  string
}

func jsonConfig(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

// AgentSetups returns the config for the common MCP clients.
func AgentSetups(mcpURL, token string) []AgentSetup {
	auth := map[string]string{"Authorization": "Bearer " + token}
	return []AgentSetup{
		{"Claude Code", "Run once in your project folder", `claude mcp add --transport http fix-assistant ` + mcpURL + ` --header "Authorization: Bearer ` + token + `"`},
		{"Cursor", "~/.cursor/mcp.json (all projects) or .cursor/mcp.json (this project)",
			jsonConfig(map[string]any{"mcpServers": map[string]any{"fix-assistant": map[string]any{"url": mcpURL, "headers": auth}}})},
		{"VS Code (Copilot agent mode)", ".vscode/mcp.json",
			jsonConfig(map[string]any{"servers": map[string]any{"fix-assistant": map[string]any{"type": "http", "url": mcpURL, "headers": auth}}})},
		{"Windsurf", "~/.codeium/windsurf/mcp_config.json",
			jsonConfig(map[string]any{"mcpServers": map[string]any{"fix-assistant": map[string]any{"serverUrl": mcpURL, "headers": auth}}})},
		{"Scripts (no MCP)", "Plain JSON API with the same token",
			strings.Join([]string{
				`curl -H "Authorization: Bearer ` + token + `" ` + strings.TrimSuffix(mcpURL, "/mcp") + `/agent/v1/issues`,
				`curl -H "Authorization: Bearer ` + token + `" ` + strings.TrimSuffix(mcpURL, "/mcp") + `/agent/v1/issues/<id>`,
				`curl -X POST -H "Authorization: Bearer ` + token + `" -d '{"summary":"...","files":["..."]}' ` + strings.TrimSuffix(mcpURL, "/mcp") + `/agent/v1/issues/<id>/fix`,
				`curl -H "Authorization: Bearer ` + token + `" "` + strings.TrimSuffix(mcpURL, "/mcp") + `/agent/v1/issues/<id>/check?reproduced=true"`,
			}, "\n")},
	}
}

// LoopPrompt starts the loop once the agent is connected.
func LoopPrompt() string {
	return "Use the fix-assistant tools to fix this app's production errors.\n\n" +
		"Start with list_issues and take the most important issue. Then follow the loop the tools describe: " +
		"get_issue, make that one change, deploy it (ask me first if you can't deploy yourself), report_fix, " +
		"then check_fix until it says looks_fixed, came_back or needs_human. If it came back, get_issue again and try the next attempt. " +
		"Work on one issue at a time, and after each one tell me in a sentence what was wrong and what you changed."
}

// Deeplink is a one-click "add this MCP server" link for an editor.
type Deeplink struct {
	Name string
	URL  template.URL // custom schemes (cursor:, vscode:) need marking as safe
}

// AgentDeeplink is the install link for the tool a token was made for:
// Cursor and VS Code open their "add MCP server" prompt with the token
// filled in. "" for tools without one.
func AgentDeeplink(tool, mcpURL, token string) *Deeplink {
	headers := map[string]string{"Authorization": "Bearer " + token}
	switch tool {
	case "cursor":
		cfg, _ := json.Marshal(map[string]any{"url": mcpURL, "headers": headers})
		return &Deeplink{"Cursor", template.URL("cursor://anysphere.cursor-deeplink/mcp/install?name=fix-assistant&config=" +
			url.QueryEscape(base64.StdEncoding.EncodeToString(cfg)))}
	case "vscode":
		cfg, _ := json.Marshal(map[string]any{"name": "fix-assistant", "type": "http", "url": mcpURL, "headers": headers})
		return &Deeplink{"VS Code", template.URL("vscode:mcp/install?" + uriComponent(string(cfg)))}
	}
	return nil
}

// agentTools are the one-click connect buttons: tool id and token name.
var agentTools = []struct{ ID, Name string }{{"cursor", "Cursor"}, {"vscode", "VS Code"}, {"claude", "Claude Code"}}

func agentToolName(id string) string {
	for _, t := range agentTools {
		if t.ID == id {
			return t.Name
		}
	}
	return ""
}

// uriComponent encodes like JavaScript's encodeURIComponent: spaces become
// %20, not "+". VS Code decodes the link with decodeURIComponent, which
// would leave a "+" in "Bearer fa_...".
func uriComponent(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }

# Agent loop (MCP)

2026-11-25 · **Built and verified locally**, including a live loop over HTTPS against a real Node.js app.

The founder's coding agent connects to Fix Assistant directly and works an issue until the fix holds. In a loop, it reads the evidence, makes the change, reports it after deploying, and asks whether the error stopped. When it hasn't, the agent gets the next attempt's prompt, which includes the new evidence and what was already tried. After 3 failed attempts it stops and hands over to a person.

## How a founder connects

1. **Create a token.** Setup → "4. Let your AI agent fix issues in a loop" → **Create an agent token**.
   - The token (`fa_…`) is shown once and only its SHA-256 is stored.
   - It can be revoked at any time, and "last used" is shown.
2. **Paste the setup for their tool.** The page shows it with the token already filled in:
   - Claude Code: `claude mcp add --transport http fix-assistant <APP_PUBLIC_URL>/mcp --header "Authorization: Bearer fa_…"`;
   - Cursor (`mcp.json`);
   - VS Code (`.vscode/mcp.json`);
   - Windsurf;
   - plain `curl` for scripts.
3. **Paste the loop prompt** into the agent: "Use the fix-assistant tools to fix this app's production errors…".

## The tools ([internal/agentapi](../internal/agentapi))

| Tool | Does |
| --- | --- |
| `list_issues` | Open work first (new, explained, came back), then fixes being verified, by impact |
| `get_issue` | Shows what broke, who is affected and why, the fix prompt for the current attempt, the latest occurrences with their version, the fixes applied so far, and the next step. It writes the prompt on the spot if the issue wasn't explained yet or a fix failed since the last prompt |
| `report_fix` | Takes a summary, the changed files and an optional release. It records a `fix_attempts` row and sets the issue to verifying from now |
| `check_fix` | The quick check (below). `reproduced=true` marks that the agent just repeated the failing action |

- **Transport:** MCP Streamable HTTP at `POST /mcp`.
  - Each request gets one JSON response, with no SSE stream and no sessions.
  - Notifications get 202, and `GET` and `DELETE` get 405.
  - Batches are accepted, and protocol versions 2025-06-18, 2025-03-26 and 2024-11-05 are supported.
- **The loop is described to the agent** in `initialize.instructions`.
- **The same operations are available as JSON** under `/agent/v1/issues[/{id}[/fix|/check]]`.
- **Every call** is scoped to the token's project. Another project's issue answers "no such issue".

## The quick check ([verify/loop.go](../internal/verify/loop.go))

The full rule (20 sessions and 48 hours) is far too slow for an agent waiting on an answer. The quick check answers within minutes, and the full rule still decides "Fixed".

| State | When |
| --- | --- |
| `not_reported` | No fix reported yet |
| `waiting_for_deploy` | Browser issue: no new site version seen since the report (or the named version isn't seen yet) |
| `watching` | Deployed and no repeats, but nobody has used the page yet. The agent is asked to reproduce the failing action |
| `came_back` | The error happened again on the fixed version: on that release for browser errors, since the report for server errors |
| `looks_fixed` | No repeats, plus at least 3 sessions on the page, or 30 s after the agent reproduced the action |
| `fixed` | Verified by the full rules, or by the founder |
| `needs_human` | 3 fixes didn't hold. `report_fix` is refused from then on |

- **Server issues** don't wait for a release name, because many servers keep one name and a working fix sends no errors that would reveal a new one. They watch from the report time instead. The rolling-deploy rule from [server-errors.md](server-errors.md) still applies.

## Attempt memory

- **Every applied fix is a `fix_attempts` row.** That includes the founder's "I applied the fix", which is recorded as source `founder`.
- **The outcome is set automatically:**
  - **came back:** grouping reopens the issue and records an implicit "failed";
  - **held:** verification records "worked";
  - **the founder's answer** also sets it.
- **The next prompt** lists the failed attempts' summaries and files under "Already tried… don't repeat these; find what they missed".
- **Attempt numbers are now correct beyond 2:** one more than the fixes that didn't hold. Before this, every regression was "attempt 2".
- **The issue page** shows "Fixes applied": each attempt, who made it, when, its summary and files, and whether it held, came back or is being checked.

## Fixed along the way

- **New site versions were only noticed when an error arrived from them.** A working fix (no errors) therefore never started verification. Page views now record their version too (`SeeRelease`; it doesn't count as an event).
- **Exits and funnel steps** depended on row order when several page views shared one timestamp, which happens within a single batch. Insertion order now breaks the tie. This also fixed a flaky analytics test.
- **App versions in explanations** now show without our `server:` marker and up to 20 characters ("app version api@1.4.2").

## Verified

- **Tests** ([agentapi_test.go](../internal/agentapi/agentapi_test.go), [verify_test.go](../internal/verify/verify_test.go), [web/app_test.go](../internal/web/app_test.go)):
  - **Protocol:** tokens (missing, wrong, revoked, another project's), initialize and version negotiation, notifications, batches, unknown methods and tools.
  - **The whole loop** on a .NET fixture: explain on demand, report, reproduce, looks fixed, came back, attempt 2 with what was tried, 3 failures, `needs_human` and refusal.
  - **Quick check on a browser issue:** waiting for deploy, watching, looks fixed after 3 sessions, an old tab doesn't count, came back.
  - **Token UI:** shown once, `no-store`, placeholder afterwards, revoke.
- **Live:**
  - A Node.js test client used MCP through the public tunnel against the "Spike app" project and its Express server.
  - A deliberately wrong fix (attempt 1, `api@1.4.3`) was reported as `came_back` seconds after deploying.
  - Attempt 2's prompt included the new occurrences and attempt 1's summary.
  - The real fix (`api@1.4.4`, return 404 for a missing order) was deployed, the failing requests were repeated, and 30 s later the state was `looks_fixed`.

## Not done

- **Connecting a real Cursor or Claude Code session:** the protocol was exercised by a test client making the same calls. The first real client may need small adjustments.
- **Browser builders (Lovable, Bolt):** I haven't confirmed whether they can call MCP servers. Their founders can still use the same loop by hand: "I applied the fix" and the quick check.
- **Per-token rate limiting and an audit view of agent actions** beyond the attempt history.
- **Deploying is the agent's or the founder's job.** Fix Assistant never deploys.

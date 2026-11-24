# Phase 4: Explain and alert

2026-11-23 · Gate: **prompts fix 50% or more of the golden set** → **PASS** (14/21 = 67% of the golden set proven fixed; 12/21 = 57% counting only real-cause errors), with the caveats below.

## The decision: the founder's agent does the reasoning

Phase 4 runs in **agent mode**, with no LLM of our own. The founder's AI agent (Lovable, Bolt, Cursor…) can read their code, which we can't until the Phase 7 GitHub integration. So we supply what needs no model, and hand the code-level work to their agent:

- **From data:** what broke, who is affected (people, times, page, first version, whether it came back), and severity.
- **A catalog of 20 known errors:** each has a plain-language "why" and a targeted single-change fix prompt.
  - PostgREST: 22001, 42501, PGRST205/42P01, PGRST116, 23505, 23502, 23503, 22P02.
  - Supabase Auth: invalid_credentials, weak_password, email_not_confirmed, user_already_exists, email rate limit.
  - Network: local address, unreachable.
  - JS: null property, not a function, not defined, chunk load, JSON parse.
- **Everything else** gets an evidence-packed "diagnose and fix" prompt: every failing request or error we saw, then "explain in one sentence, then fix only that".
- **Agent adapters:** product framing for Lovable, Bolt, Replit and v0; codebase-search framing and a diff request for Cursor; the plain base prompt for "other". The founder can switch agents on any issue.

No API key, no per-issue cost, no error data sent to an extra AI vendor. The FDR's `LLM` and `Embedder` interfaces remain (`internal/ai/llm.go`), so our own model can be switched on later for errors outside the catalog.

## Gate result

**Golden set:** 21 cases in `internal/ai/golden/cases.json`. Eight come from the spike app's real captured errors, twelve are realistic Supabase and React failures with real-shaped bodies, and one is a regression case found while building this. `app eval` checks each against a rubric:
- the right catalog entry;
- required mentions (the exact table, column or property);
- forbidden advice, such as disabling RLS or dropping the unique rule;
- what broke, who and how to check all present;
- ends with how to check, ≤ 2000 characters, never asks for a rewrite.

**21/21 pass for all six agent styles.** This also runs in CI as `TestGoldenSet`.

**Proof of fixes** (`spike/harness/fixcheck.mjs`). The spike app reproduces 15 of the cases, against a local PostgREST serving the broken schema (the same server Supabase uses, so the error bodies are real) and hosted Supabase for auth.
1. **Before:** run every case and capture the errors.
2. **Prompts:** generate them from those captures through the production path (`app eval --from-envelopes`); they're in `spike/fixcheck/prompts.md`.
3. **Fix:** acting as the founder's agent, I applied **only what each prompt said**, to a copy of the app (`spike/fixcheck/app`) and as migrations (`spike/fixcheck/fixes`).
4. **After:** run again.

| Case | Prompt | Before | After |
|---|---|---|---|
| js_typeerror | js.null_property | error | **fixed** |
| js_custom | diagnose & fix | error | **fixed**\* |
| js_rejection | diagnose & fix | error | **fixed**\* |
| net_22001 | pg.value_too_long → migration to `text` | error | **fixed** (45-char email saved) |
| net_42501 | pg.rls_denied → policy + ask to log in | error | **fixed** (visitors asked to log in; RLS still on) |
| net_auth | auth.invalid_credentials → message + reset link | no message | **fixed** (message shown; the 400 is expected) |
| net_404 | pg.missing_table | error | **fixed** |
| net_dead | net.local_address → env var + message | error | **fixed** |
| pg_unique | pg.unique_violation → upsert | error | **fixed** (one row, no duplicate) |
| pg_not_null | pg.not_null | error | **fixed** (row saved with name) |
| pg_invalid_uuid | pg.invalid_input | error | **fixed** |
| pg_single | pg.single_row → maybeSingle | error | **fixed** |
| auth_weak_password | auth.weak_password → validate first | error | **fixed** |
| js_map | js.not_a_function | error | **fixed** |
| js_json | (no event captured, see below) | not reproduced | — |

\* These two are deliberately thrown errors with no real underlying bug, so "fixed" is trivial. Counted separately above.

**Score:** 14 of 21 golden cases are proven fixed (67%), or 12 of 21 (57%) counting only real-cause errors. The other 7 aren't reproducible in the spike app (foreign key, not defined, chunk load, edge function, Safari, local-proxy regression, `js_json`) and count as unproven, not fixed.

## Caveats (read before trusting the number)

1. **I wrote the prompts *and* acted as the agent.** That's biased towards success. The real test is founders pasting prompts into Lovable on partner apps. Phase 5's feedback buttons and implicit verification measure exactly that.
2. **The logged-in half of the RLS fix wasn't tested.** There's no local auth server; the anonymous half was.
3. **The spike app is small.** Real codebases make "find where `.profile` is read" harder.

## Bugs found and fixed on the way

- **The scrubber broke JSON bodies.** The secret rule matched `password` inside GoTrue's `"weak_password":{…}`, replaced the object, made the body invalid JSON and lost the error code. Both scrubbers (browser and server) now match whole key names only, never replace objects or arrays, and keep scrubbed string values quoted. There are tests for both.
- **`net.local_address` fired on any localhost host.** Now it only fires when nothing answered.
- **Minified variable names leaked into prompts** ("find `hg.items.map`"). The prompt now uses the searchable property path (`items.map`).
- **Re-analysis moved regressed issues out of "Came back".** Regressed issues now stay `open` with their second-attempt prompt.
- **Alert counts.** Duplicate digests were reported as sent; `record` now says whether it created the alert.

## Open items

- **The Sentry SDK doesn't report rejections from `response.json()`** ("Unexpected token '<'"). A plain `JSON.parse` rejection is reported; this one isn't. It's a common failure in AI-built apps, so the add-on should catch it (Phase 5).
- **Alert emails have no "Worked / Didn't work" links yet.** Those come with Phase 5 feedback.
- **80% and 100% monthly-quota emails** to founders aren't built yet.
- **No embeddings are computed yet.** `knowledge_entries.embedding` is a `real[]` and stays empty until a provider (Voyage or other) is chosen. Entries store only the signature and template id, never samples.
- **The catalog is hand-written.** Grow it from partner data (Phase 7 automates this).

## What was built

- `internal/ai`:
  - the context builder, which collects up to 3 scrubbed samples plus counts, page, release and regressions;
  - the catalog, `Explain`, `ForAgent`, `Analyze`, and the golden set with its eval.
- `internal/alerts/notify.go`: instant-alert rules, the digest (hourly sweep, 09:00 project time, skipped when nothing is new), idempotent per issue/day and project/day. Every alert is stored and shown in the dashboard, and emailed unless email is off.
- **Jobs:** `analyze_issue` (queue `ai`, unique per trigger), `alert_check` (queue `alerts`), `daily_digest` (hourly). `group_event` enqueues analysis on the first threshold crossing and on regression.
- **Migration 00003:** `fix_suggestions`, `knowledge_entries`, `alert_log`, `issues.explained_at`, `projects.timezone`.
- **Dashboard:** the issue page shows what broke, who, why, a copyable fix prompt with the agent switcher, how to check, "Explain now" or "Refresh explanation", and an Alerts tab.
- **CLI:** `app eval [--agent] [--show NAME|all]` and `app eval --from-envelopes DIR`.

## Re-running the fix check

```sh
# PostgREST needs the Postgres bin folder on PATH (for libpq.dll)
PATH="$LOCALAPPDATA/Programs/pg16/pgsql/bin:$PATH" "$LOCALAPPDATA/Programs/postgrest/postgrest.exe" spike/fixcheck/postgrest.conf &
go build -o bin/app.exe ./cmd/app
cd spike
node harness/fixcheck.mjs before
../bin/app.exe eval --from-envelopes ../tmp/fixcheck/before/envelopes > fixcheck/prompts.md
node harness/fixcheck.mjs after
node harness/fixcheck.mjs report
```

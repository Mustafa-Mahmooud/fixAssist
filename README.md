# Fix Assistant

One Go binary + Postgres that turns browser errors into plain-language explanations and fix prompts for AI-built apps. Design: the FDR "AI Fix Assistant — Go Monolith" (2026-11-22).

**Status: Phases 1–7 built and verified locally (billing postponed).** Errors are grouped, explained with a fix prompt for the founder's own AI agent, alerted and verified after the next release; usage analytics, funnels with an error overlay and weekly growth insights; a cross-app knowledge base (proven fixes, automatic quarantine of failing ones) and a GitHub App that makes prompts point to the exact file and line. The partner gates need the deployment, which is prepared ([docs/deploy.md](docs/deploy.md)) and paused at Google Cloud billing. Notes: [Phase 7](docs/phase7-notes.md), [Phase 6](docs/phase6-notes.md), [Phase 5](docs/phase5-notes.md), [Phase 4](docs/phase4-notes.md), [Phase 3](docs/phase3-notes.md), [Phase 2](docs/phase2-notes.md), [Phase 1](docs/spike-findings.md).

## Layout

```
cmd/app/                       binary: serve, migrate, retention, admin, spike-capture
internal/ingest/               envelope parser, key auth, quotas, server scrub, Prepare, PGSink, FileSink
internal/normalize/            message/URL/filename normalization, fingerprints, signatures
internal/normalize/bodyparse/  PostgREST, Supabase Auth, Firebase, generic JSON, raw
internal/projects/             orgs, projects, DSN keys, key cache, rate + monthly limits
internal/grouping/             issue upsert, users, hourly counts, samples, releases, regressions, impact
internal/verify/               fix feedback (explicit + implicit) and the verification rules
internal/analytics/            visitor identity, GeoIP, rollups, funnels, weekly growth insights
internal/github/               GitHub App: manifest, install verification, repo scan for file-level prompts
internal/obs/                  Prometheus metrics (/metrics)
internal/jobs/                 River client, workers (group_event, partitions); jobargs/ holds job args
internal/auth/                 magic links, sessions, CSRF
internal/ai/                    issue context, error catalog, fix prompts, agent adapters, golden set + eval
internal/alerts/               email Sender (dev .eml / Resend), instant alerts, daily digest
internal/store/                pgx pool, goose migrations, sqlc queries (store/db), partitions
internal/web/                  dashboard (templates + htmx, embedded) and GET /sdk/{key}.js
pkg/sdk-addon/                 add-on source + built dist/ + pinned Sentry 8.55.0, embedded in the binary
spike/                         stand-in Lovable app, Supabase migration, Playwright harness
```

## Requirements

Go 1.26 (auto-downloaded by Go 1.24+), Node 22+, Postgres 16. Tools: `sqlc` 1.29 (only to regenerate `internal/store/db`).

## Local Postgres without Docker

```sh
# Postgres 16 Windows binaries in %LOCALAPPDATA%\Programs\pg16, data in %LOCALAPPDATA%\fixassist\pgdata
pg_ctl -D "%LOCALAPPDATA%\fixassist\pgdata" -l "%LOCALAPPDATA%\fixassist\postgres.log" start
# databases: fix (dev), fix_test (integration tests; its schema is wiped by the tests)
```

## Run

```sh
export DATABASE_URL=postgres://fix@127.0.0.1:5433/fix
go build -o bin/app.exe ./cmd/app

./bin/app.exe migrate up
./bin/app.exe admin create-project --org "My Org" --name my-app --agent lovable --backend supabase --domains myapp.lovable.app
#  -> prints the project id, DSN and the one script tag to install
./bin/app.exe serve            # roles ingest,web,worker on 127.0.0.1:8787
./bin/app.exe admin list-projects
./bin/app.exe admin rotate-key --project 1
./bin/app.exe admin set-quota --project 1 --quota 100000
./bin/app.exe retention run    # create upcoming partitions, drop those > 30 days
./bin/app.exe eval             # golden-set rubric for the fix prompts (--show all prints them)
```

Open http://127.0.0.1:8787/login. Without `RESEND_API_KEY` the sign-in email is written to `tmp/mail/*.eml` and its link is logged.

Env: `INGEST_PUBLIC_URL` (host in DSNs and script tags), `APP_PUBLIC_URL` (dashboard URL in sign-in links; https turns on Secure cookies), `APP_SECRET` (signs email feedback links), `STAFF_EMAILS` (who can open `/admin`), `METRICS_TOKEN`, `GEOIP_DB` (DB-IP Lite .mmdb for countries), `TRUST_PROXY=1` (behind Caddy), `GITHUB_APP_*` + `GITHUB_CLIENT_*` (optional code context), `RESEND_API_KEY` + `EMAIL_FROM`, `EMAIL_MODE=off`, `EMAIL_DEV_DIR`. All default to local values.

## Tests

```sh
go test ./...                                    # unit tests + Phase 1 gate (golden files)
# Postgres tests too. Each package resets fix_test, so run packages one at a time:
FIX_TEST_DATABASE_URL=postgres://fix@127.0.0.1:5433/fix_test go test -p 1 ./...
```

## Spike harness

Needs a Supabase backend in `spike/.env` (`SUPABASE_URL`, `SUPABASE_ANON_KEY`) and `npm install` in `pkg/sdk-addon`, `spike/app`, `spike/` (+ `npx playwright install chromium`).

```sh
# Golden files (Phase 1): no database
./bin/app.exe spike-capture &
cd spike && npm run harness

# Live run: real server + database (key from the setup page or create-project)
./bin/app.exe serve &
cd spike && HARNESS_MODE=db FIX_PUBLIC_KEY=<key from create-project> npm run harness
```

After editing `pkg/sdk-addon/addon.js`, run `npm run build` there and rebuild the binary (the add-on is embedded).

## Dashboard tour

`node spike/harness/ui-tour.mjs signup` signs up through the dashboard (dev mailer), creates "Spike app" and prints its key; after sending events, `ui-tour.mjs screens` saves screenshots of every page (desktop, and phone in dark mode) to `tmp/screens`.

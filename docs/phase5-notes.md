# Phase 5: Verify and harden (MVP complete)

2026-11-23 · **Built and verified locally. The MVP feature set from the FDR is complete.** The gate is "about 50% of suggestions on partner apps marked worked, and founders act on the emails every week". It is **not run yet**: it needs real partners, which needs the deployment (paused at Google Cloud billing linking). The admin page shows the number the gate needs.

## What was built

**Closing the loop** (`internal/verify`, FDR "Fix feedback and verification"):

| Rule | Implementation |
|---|---|
| 1. explained → verifying | `verify_fixes` job (every 15 min): a release first seen after the suggestion; or the founder clicks **I applied the fix** (then it watches the next release) |
| 2. verifying → fixed | On the watched release: the issue's page has ≥ 20 sessions, ≥ 48 h have passed, and the fingerprint has 0 events. Writes `worked/implicit` |
| 3. verifying or fixed → open | In grouping: the fingerprint shows up on the **watched release or a newer one**. Writes `failed/implicit` and triggers a second-attempt explanation |
| 4. Low traffic | 7 days without 20 sessions sets "probably fixed?" and asks the founder; nothing implicit is written |
| 5. Explicit answers win | **Worked**, **Didn't work** and **Partly** on the issue page and in email; "didn't work" immediately writes a second-attempt prompt |

Every feedback row updates its `knowledge_entries` counts and confidence, (worked + 1) / (worked + failed + 2).

**Email feedback links:** instant alerts carry signed **Worked / Didn't work** links: HMAC with `APP_SECRET`, valid for 30 days, and tampering is rejected. Opening a link shows a confirm button instead of recording on GET, because email security scanners open links automatically; the magic links work the same way.

**Quota emails:** at 80% and 100% of the monthly event quota, once per month per threshold. They're stored in the Alerts tab and emailed when email is on.

**Admin** (`/admin`, only for emails in `STAFF_EMAILS`, everyone else gets a 404):
- prompt outcomes per catalog entry and overall, the gate metric;
- ingest per project for the last 1 h and 24 h;
- ingest response codes, including 429s;
- queue depth, lag and recent failures;
- AI cost (zero in agent mode);
- raw issue JSON at `/admin/issues/<id>`.

**Metrics** (`/metrics`, Prometheus):
- `fix_http_requests_total{endpoint,code}`;
- `fix_http_request_duration_seconds` (FDR ingest target: p99 < 50 ms);
- `fix_queue_jobs{queue,state}`;
- `fix_queue_lag_seconds` (FDR: split ingest out when grouping lag stays over 60 s);
- Go and process stats.

It answers only localhost or `Authorization: Bearer $METRICS_TOKEN`, and Caddy returns 404 for `/metrics` from the internet.

**SDK:** the add-on now reports `response.json()` failures with the response's URL, status and content type. The pinned Sentry SDK missed these rejections, and they're a common failure in AI-built apps. The catalog prompt names the exact endpoint and type ("expects JSON from `/api/report.json` but gets text/html"). The add-on is 2.6 KB gzipped (it was 2.4).

## Verified

- **Full suite** (`go test -p 1 ./...` against Postgres) passes, including the new tests:
  - `verify`: rule 1; not fixed before 48 h, fixed after; probably-fixed on low traffic without writing feedback; a stale tab on the old release doesn't reopen but the watched release does; explicit answers; "I applied the fix"; confidence math (2/3 and 1/3);
  - `web`: the email link flow (confirm page, GET doesn't record, tampered and expired links rejected, "didn't work" writes attempt 2); `/admin` staff-only, with the link hidden from others;
  - `obs`: request counting; `/metrics` access (localhost and token yes, internet no).
- **Golden set** is 22/22. A new case covers the `response.json()` context.
- **Fix check:** 15/15 reproduced spike cases fixed by following the generated prompts. `js_json` is now captured and fixed; in Phase 4 it was "not reproduced". Phase 4's caveats still apply: I wrote the prompts and acted as the agent.
- **Live:** the local server runs with all roles. `verify_fixes` and the digest/quota sweep run on start, then every 15 minutes and hourly.

## Bugs found on the way

- **The instant-alert cap could be exceeded.** Two `alert_check` workers could both count "2 sent today" and both send. That caused the intermittent test failure seen twice. The count and insert now run in one transaction under a per-project advisory lock, with email sent after commit.
- **The admin outcome query was invalid SQL** (an ungrouped column), and my first rewrite would have undercounted equal costs (`sum(distinct …)`). Feedback is now aggregated per suggestion before the join.
- **Phase 4's regression rule would have reopened issues from stale tabs.** Any event on a release other than the fixed one reopened the issue, including people still on the *old* version. Reopening now requires the watched release or a newer one.

## Decisions and deviations

- **Email links need a confirm click** (the FDR says "one click"). Scanners would otherwise record answers on their own.
- **"Partly"** records `partial` feedback and keeps the issue verifying. It doesn't move confidence.
- **Verification uses the issue's page route** to count sessions on the new release. Issues with no route never reach 20 sessions, so they end at "probably fixed?" (rule 4) or an explicit answer.
- **No Prometheus server or dashboards are deployed** (free tier). `/metrics` is ready for one; the admin page covers the day-to-day numbers.
- **The quota and daily cap are still per process / per database row.** That's fine on one VM; see Phase 2 notes for multiple boxes.

## Not done (needs the deployment)

- **Backups and the restore test:** `deploy/backup.sh` and `deploy/restore-test.sh` exist and are scheduled by `bootstrap.sh`, but they've never run against a real R2 bucket.
- **Uptime check:** an external monitor on `https://<domain>/healthz` (see `docs/deploy.md`).
- **Dogfooding:** installing our own SDK on the dashboard (FDR).
- **The Phase 5 gate itself:** partner apps, then the share of suggestions marked worked (Admin page) and weekly founder action (Alerts tab and feedback history).

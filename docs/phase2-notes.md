# Phase 2 — Ingest and store

2026-11-22 · Gate: **your own apps send clean, scrubbed events** → **PASS** (spike app, both builds, through the real server into Postgres).

## Gate run

The spike app loaded the SDK from the one served script tag (`/sdk/{key}.js`) and sent all 8 error cases in two builds to `app serve` (ingest, web and worker roles). Project 1 was created with `app admin create-project … --domains localhost`.

| Check | Result |
|---|---|
| Envelopes accepted | 16/16 `200`; page views 4/4 `204` |
| Stored | 16 events in `events_p20261122` (6 js, 10 net); 4 page views; usage counter = 16 |
| Fingerprints | 8 distinct (one per case), identical across builds |
| Affected users | 2 user hashes (one per browser tab, i.e. one anonymous session each), 32 hex chars, HMAC per project |
| Jobs | 16 `group_event` completed (stub marks `grouped_at`); `partitions` ran on start |
| PII in stored payloads | 0 rows match emails, JWTs, the test password, raw session or user IDs; query values are blank |

The end-to-end tests (`internal/ingest/pipeline_test.go`, against `fix_test`) cover the same path plus every rejection:

- an unknown key returns 401;
- a key used with the wrong project ID returns 403;
- a foreign origin returns 403;
- a rotated key stops working once the cache refreshes;
- going over the monthly quota returns 429 with `Retry-After` and `X-Sentry-Rate-Limits`;
- over the sample cap, the row is still stored but without its payload;
- page views are stored with route templating;
- old partitions are dropped, and running retention twice does nothing the second time.

## What was built

- **Ingest path**: key lookup (in-memory cache, refreshed every 30 s, misses fall through to one query plus a 30 s negative cache) → project-ID and allowed-domain checks → per-key token bucket (50/s, burst 200) and monthly quota → server scrub → `Prepare` (user hash, kind, fingerprint, route, release) → per-fingerprint sample cap (100/hour) → **one transaction**: event rows + monthly usage + `group_event` jobs (River `InsertManyTx`).
- **Server scrub** (`internal/ingest/scrub.go`) repeats the browser rules, with Luhn-checked cards. It also drops cookies, request bodies, auth headers, user email and username, and typed-input breadcrumbs, and truncates IPs to /24 (IPv6 to /48). Raw user IDs and session IDs are replaced by `user.hash`.
- **Schema**: goose migration plus River's migrations, run under one advisory lock at startup. `events` and `pageviews` use daily range partitions; the `partitions` job creates the next 7 days and drops anything older than 30 days.
- **SDK endpoint**: `GET /sdk/{key}.js` serves the pinned Sentry bundle, the DSN config line and the add-on, all embedded in the binary, with an ETag and a 5-minute cache.
- **Admin CLI**: `create-project`, `list-projects`, `rotate-key`, `set-quota`; also `migrate up` and `retention run`.

## Decisions and deviations from the FDR

1. **Fingerprints are computed at ingest, not in `group_event`.** The per-fingerprint sample cap has to know the fingerprint before deciding whether to store the payload, and normalization is cheap and pure. `group_event` (Phase 3) does the issue upsert using the stored fingerprint.
2. **`bigserial`, not identity**, on `events`/`pageviews`: Postgres 16 doesn't support identity columns on partitioned tables.
3. **The event timestamp is truncated to microseconds** before it goes into the job args, so the `(id, received_at)` pair matches the stored row exactly.
4. **Monthly quota is counted in memory** and merged with the database counter on each cache refresh (the larger value wins). A crash loses at most about 30 s of counting. With several ingest boxes the quota becomes approximate; it would need a shared counter then. The founder emails at 80% and 100% are part of alerts (Phase 4).
5. **Sample cap and rate buckets are per process.** That's fine on one VM; revisit when ingest moves to its own box.
6. **Anonymous identity is the add-on's per-tab session ID** (`contexts.fix.session_id`), HMAC'd like a user ID. It's random per tab and the raw value is never stored. Page views keep the session ID as is, since it isn't personal data and Phase 6 needs it to count sessions.
7. **Page views are not counted against quota** (only capped at 50 per request). Phase 6 may add a separate page-view quota.
8. **Go 1.26**: current pgx and River require it. Go 1.24+ downloads it automatically.
9. **Local dev Postgres is native, without Docker or pgvector.** pgvector is needed in Phase 4 for `knowledge_entries`; plan to use a managed Postgres or build pgvector then.

## Carried into Phase 3

- `group_event`: issue upsert on `(project_id, fingerprint)`, `issue_users`, `issue_hourly`, `issue_samples` cap, release tracking and regression detection. The job, its idempotency (`grouped_at`) and the queue are already in place.
- The grouping-collision risk from Phase 1 (same message on the same route).
- `alerts`/`web`/`auth` are still stubs.

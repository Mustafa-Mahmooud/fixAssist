# Phase 3 — Group and show

2026-11-22 · Gate: **5 partner apps live, grouping reviewed by hand** → **not yet run.** Everything the gate needs is built and verified on the spike app. The gate itself needs partners and a public deployment (decided: deploy as a separate step).

## Verified locally

A founder signed up through the real dashboard: magic link from the dev mailer, then an account and org were created. They created "Spike app" (Lovable, Supabase, domain `localhost`), copied the key from the setup page, and the spike app sent both builds through the served script tag.

| Check | Result |
|---|---|
| Issues | 8 issues from 16 events, one per error case; each has 2 events and 2 people |
| Releases | 2 detected; every issue has `first_release_id` = build 1 and `last_release_id` = build 2 |
| Samples | 16 stored (≤ 20 per issue) |
| Jobs | 32 `group_event` jobs completed (16 from Phase 2, 16 from this run) |
| Setup page | shows the Lovable install prompt and the script tag, polls every 3 s, and switches to "Connected" after the first event |
| Issue list | sorted by impact, status tabs (htmx); desktop, and phone in dark mode with no horizontal scroll |

The tests run against Postgres with `-p 1`:
- **Grouping:** golden envelopes → issues, users, hourly counts, samples, releases. A retried job doesn't count twice. Marking an issue fixed and then seeing it on the same release keeps it fixed; seeing it on a new release reopens it.
- **Dashboard:**
  - the full magic-link flow, and the GET confirmation page doesn't use up the token;
  - a link can't be used twice, and an expired link is rejected;
  - the rate limit is 5 links per email per hour;
  - CSRF is required on every POST;
  - another org's projects return 404;
  - project creation leads to the setup page and its poll;
  - issue ordering and filtering work, and sign-out ends the session.

## What was built

- **`internal/grouping`**: one transaction per event.
  1. Lock the event row and skip it if it's already grouped.
  2. Upsert the issue on `(project_id, fingerprint)`.
  3. Detect new users atomically (`insert … on conflict do nothing`) and update the issue's user count.
  4. Detect "new this hour" with a conditional update, and bump `issue_hourly`.
  5. Keep a rolling 20 samples and upsert the release.
  6. Run the regression rule, recompute impact, and set `events.issue_id`.
- **Impact** = people in the last 24 h × route weight (3 for checkout, signup, login, cart, billing… as a whole path segment; the project's `key_pages` list wins when set).
- **Analysis threshold hook**: `Result.ReachedAnalyze` is true the first time an issue crosses 2 people or 3 events. The worker calls `OnAnalyze`, which Phase 4 sets to enqueue `analyze_issue`.
- **Auth** (`internal/auth`):
  - magic links: 32 random bytes, stored as SHA-256, valid for 15 minutes, single use, 5 per email per hour;
  - the GET only shows a confirmation button; the POST signs you in, so email link scanners can't use up the token;
  - sessions: an HttpOnly, SameSite=Lax cookie (Secure when the dashboard is on https), valid for 30 days and extended when used, stored as a hash; each has its own CSRF token;
  - an account and its own org are created on first sign-in.
- **Email** (`internal/alerts`): a `Sender` interface. The dev sender writes `.eml` files and logs the link; Resend is used when `RESEND_API_KEY` is set.
- **Dashboard** (`internal/web`), using html/template and htmx (vendored 2.0.4) with one hand-written stylesheet for light, dark and phone, and a strict CSP:
  - login;
  - projects, including create;
  - setup, with an install prompt per agent, a copy button and a 3-second poll;
  - the issue list, with status tabs;
  - issue detail: stats, a 7-day hourly chart, ignore and un-ignore, plus a "details for reviewers" section with the signature, fingerprint, releases and raw samples for the hand grouping review.

## Decisions and deviations

1. **JS fingerprint v2.** It keeps identifiers that the JS engine itself quotes (`reading 'profile'`, `evaluating 'a.b'`, `'x' is not a function`) and adds the nearest function name that survived minification. It's stable across both captured builds, and it separates the cases v1 merged (unit test). Existing v1 fingerprints are kept apart by their version prefix. Network fingerprints are still v1.
2. **Release tracking lives in `grouping`**; the separate `internal/releases` stub was removed.
3. **Grouping runs in parallel and events can arrive out of order**, so the issue upsert takes the first and last release from whichever event is earliest or latest by time.
4. **Issue detail exists now**, earlier than the FDR's Phase 4, because the hand grouping review needs samples and signatures. Phase 4 adds the explanation and fix prompt to it.
5. **Not in Phase 3**: GitHub OAuth sign-in, a settings screen (key pages, domains and key rotation are still CLI-only), invites, and alerts.
6. **Test DB note**: every database test package resets `fix_test`, so run them with `go test -p 1`.

## Before the gate can run

1. **Deploy** (VM, Caddy, Postgres, backups) with `INGEST_PUBLIC_URL` and `APP_PUBLIC_URL` on https.
2. **Email**: a Resend account with a verified sending domain (SPF, DKIM, DMARC), so magic links reach real inboxes.
3. **Recruit 5 partner apps**, install them through the setup page, then review grouping by hand on each issue's detail page.

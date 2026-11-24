# Phase 6: Usage analytics and growth insights

2026-11-23 · **Built and verified locally, in four slices.** The FDR gate to move on to Phase 7 is "a few hundred solved issues logged", which needs partners in production.

The same snippet now answers the founder's three questions:
- how many people came and signed up;
- what they use;
- what to change to get more sign-ups.

Like fixes, each answer ends with a prompt for their AI agent.

## Slice 1: collection

**SDK add-on** (3.4 KB gzipped; the FDR target was ~3 KB):
- `session_start` with referrer host and UTM source, medium and campaign;
- page views on in-app route changes (`pushState`, `replaceState`, `popstate`), not only on full loads;
- click autocapture: button or link text only, up to 40 characters, never input values, and `data-fix-ignore` hides an element;
- `fix.track(name, props)` and `fix.identify(accountId)`;
- automatic **sign-up / log-in detection** from successful Supabase (`/auth/v1/signup`, `/auth/v1/token?grant_type=password`) and Firebase auth responses;
- events are batched and sent every 5 s and when the page is hidden.

**Privacy (cookieless by default, FDR):**
- **Visitor ID:** a visitor is `HMAC(daily salt, IP + user agent + project)`. The salt derives from `APP_SECRET` and the UTC date, so nobody can be followed across days. Only with `window.__fixConsent === true` does the SDK keep a first-party ID, stored as an HMAC.
- **`window.__fixConsent === false`:** errors only, no analytics.
- **What's never stored:** raw IPs, user agents and account IDs.
- **Country:** looked up locally (DB-IP Lite) before the IP is discarded.
- **Event properties:** scrubbed, with nested objects dropped.

**Storage:**
- `analytics_events` in daily partitions, kept 30 days like errors;
- page views gain visitor, country and device;
- rollup tables, kept forever: overview, page, source, click, event, dimension, goal and funnel stats, plus weekly retention;
- `rollup_hourly` recomputes today and yesterday. The FDR said "incremental"; recomputing two whole days is simpler, idempotent and picks up late events.

## Slice 2: Overview and Pages & features

- **Overview:** visitors, sessions, page views, sign-ups, sign-up rate, a daily chart, sources, devices and countries, with DB-IP attribution.
- **Pages & features:** per page, views, visitors, exit rate (overall and on phones), load time and open issues; the most-clicked buttons and links; custom events.

All screens read rollups only, over the last 7 or 30 days.

## Slice 3: goals and funnels with the error overlay

- **Goals:**
  - a **Sign-ups** goal is created automatically for every project (marked as a lead);
  - founders add page, click or event goals and can mark them as leads.
- **Funnels:**
  - ordered steps (page, click, event or sign-up), counted per session in order;
  - each step shows the conversion from the previous one, how many left there, and the phone and computer split;
  - a one-click **suggested funnel** is built from the top pages plus sign-up;
  - creating a funnel or goal backfills 30 days.
- **Error overlay** (the differentiator): each page step lists that page's open issues, and a sign-up step lists open sign-up errors, each linking to its fix prompt.

## Slice 4: weekly growth insights (rule-based, agent mode)

Every Monday at 09:00 project time (or with "Find insights now"), up to 3 insights are generated from the last 7 days:

| Detector | Fires when | Re-measured after the next release |
|---|---|---|
| Sign-up errors (always first) | an open sign-up issue exists | sign-up rate |
| Funnel drop | a step loses ≥ 50% of ≥ 20 sessions | that step's conversion |
| Phone layout | the phone exit rate is ≥ 20 points above computers (≥ 20 phone sessions) | phone exit rate |
| Slow page | average load ≥ 3 s over ≥ 20 views | load time |
| Weak source | a source with ≥ 20 sessions signs up at < half the overall rate | that source's sign-up rate |

Each insight has:
- the evidence in numbers;
- a hypothesis, worded as one;
- a single-change prompt for the founder's agent;
- how to check it;
- a **before/after check:** after the first release seen after the insight, the same metric is measured over the same 7 days. A 10% relative improvement counts as "helped", the reverse as "worse", anything else as "no change".

Founders can also answer Yes / No change / Worse themselves. A weekly summary goes to the Alerts tab, and is emailed when email is on.

## Verified

- **Tests** (`go test -p 1 ./...` against Postgres): all pass. New ones cover:
  - visitor hashing: same day stable, next day different, per project, and consented IDs stable;
  - proxy trust, and the real DB-IP file (8.8.8.8 → US);
  - end-to-end collection and rollup for three scripted visits, every number checked by hand: sources, the sign-up credited to Google, exits, load time, clicks, events, devices, the goal, a funnel of 3 → 1 → 1, retention; re-running gives the same numbers, the account is stored only as a hash, and nested or private event fields are dropped or scrubbed;
  - insight ranking and evidence text, idempotent weekly generation, the before/after check (waits for a release, then "helped" for a page that got 2× faster and "worse" for a metric that dropped), and both outcome directions;
  - every dashboard page template parses.
- **Live** on the spike app through the real server:
  - 16 clicks, 2 session starts and 4 enriched page views; same browser and day = 1 visitor, correctly;
  - the Overview, Pages and Funnels screens render, and the suggested funnel was created in one click with the error overlay listing the "/" page's open issues;
  - no phone overflow, in dark mode;
  - no insights, correctly: the spike app has 4 sessions and each detector needs 20.

## Found and fixed on the way

- **Ranking would have hidden broken sign-ups.** Weighting by sessions put a 5-person sign-up error behind traffic-heavy ideas, so sign-up errors are now pinned first.
- **Four template functions were missing.** A scripted edit failed to match after gofmt re-aligned the line, and the build couldn't catch it because templates are parsed at startup. A new test now fails if any page doesn't parse, so CI catches this.
- **The partition retention test** hard-coded 2 tables; it now follows the partitioned-table list.

## Decisions and deviations

- **Insights are rule-based, not LLM-written** (agent mode). Each detector says why it fired in numbers. An LLM can be added later behind the existing interface, and should only ever get aggregates (FDR).
- **"New vs returning" and retention need consent.** Cookieless visitors change every day by design. Retention uses `fix.identify` accounts, and is merged with `greatest()` so weeks survive the 30-day raw retention.
- **Visitors are summed per day** over a range, so the same person on two days counts twice. The Overview says so.
- **Page views and analytics aren't counted against the monthly event quota.** Each request is capped at 50 events. A separate analytics quota may be needed at scale (the FDR's ClickHouse trigger is about 50M events/month).
- **GeoIP:** DB-IP Lite, downloaded at bootstrap and refreshed monthly by `deploy/geoip.sh`. Without the file, countries are simply empty.

## Open items

- **Real traffic is needed** to tune the thresholds: 20 sessions, a 50% drop, a 20-point phone gap, 3 s, half the conversion rate.
- **The add-on is 3.4 KB gzipped,** above the ~3 KB target. It could be trimmed by moving the analytics client into its own lazily loaded file.
- **There's no settings screen yet** for timezone, key pages and domains (they're still CLI or database only).

# Speed (Core Web Vitals)

The **Speed** screen shows how fast each page feels to visitors, measured in their own browsers. There's nothing to set up: the snippet measures it.

## What's measured

| Metric | Shown as | Good | Poor |
|---|---|---|---|
| LCP, Largest Contentful Paint | Loading: until the main content shows | ≤ 2.5 s | > 4 s |
| INP, Interaction to Next Paint | Responsiveness: until a tap or click shows a result | ≤ 200 ms | > 500 ms |
| CLS, Cumulative Layout Shift | Layout shift: how much the page jumps | ≤ 0.1 | > 0.25 |
| FCP, First Contentful Paint | First paint (whole app only) | ≤ 1.8 s | > 3 s |
| TTFB, Time to First Byte | Server response (whole app only) | ≤ 0.8 s | > 1.8 s |

Each number is the **75th percentile**, as Google reports it: three out of four visits were at least this fast. A page needs at least 20 measurements before it gets a number and a rating.

Chrome and Edge report all of these. Safari and Firefox report only some (no LCP or INP in Safari), so a page can have fewer measurements than visits.

## How

- **SDK.** `pkg/sdk-addon` bundles Google's [web-vitals](https://github.com/GoogleChrome/web-vitals) 4.2.4 (pinned, attribution build), so the rules match Chrome's: LCP stops at the first input, CLS uses session windows, INP is the worst interaction, and back/forward-cache restores count as new page loads.
  - Each metric is sent once per page load, as an analytics event of type `vital`, for the page it loaded on. In single-page apps, INP and CLS cover the whole visit from that page.
  - It's sent with the usual analytics batch. `window.__fixConsent = false` turns it off along with the rest of analytics.
- **Attribution, for fix prompts:**
  - The element, as a CSS selector with no text.
  - The resource or script URL, with query values dropped in the browser.
  - Where the time went: LCP's server response, load delay, download and render delay; INP's input delay, processing and presentation delay.
- **Ingest** drops unknown metrics and out-of-range values. Vitals don't count toward the monthly error quota.
- **Storage.**
  - Raw events stay 30 days, like all analytics.
  - The daily rollup writes `daily_vitals`: a histogram per day, page, device, release and metric, using `analytics.VitalEdges`.
  - Every good/poor threshold is a bucket edge, so ratings from buckets are exact. Percentiles are computed from the buckets, so any range of days, devices or releases combines correctly.
- **"What's slowing it"** is the most common element and resource among the page's slow measurements (over the "good" threshold) in the raw events. Visitors' browsers report it, so it's shown as data and never followed as instructions.

The Pages screen's **Full load (avg)** is a different, older number: the average time until the whole page finished loading, on the first page of a visit.

## Checking the SDK

Go tests can't run browser APIs. After changing the add-on or upgrading web-vitals, rebuild it (`npm run build` in `pkg/sdk-addon`) and run the browser check:

```sh
cd spike && node harness/vitals-check.mjs
```

It loads a test page in Chrome (via Playwright) with a slow image, a slow button and a late banner. It passes when all five metrics arrive, attributed, with no query values and no page errors.

## Slowdowns become issues

`internal/perf` runs every hour (the `perf_check` job). For each page, metric (LCP, INP, CLS) and device class (phones, computers), it compares the page's current period with the one before:

- **By release:** when the page serves a new release. The SDK's release id hashes the page's own scripts, so pages of one site can differ. Everything measured on the new release is compared with the release the page served just before. If that release has too few measurements, the two weeks of earlier releases are used instead.
  - A release is judged only once it has been live for 12 hours, because cold caches make the first visits slow.
  - The "current" release is the one most visits got in the last two days, so a rollback counts as a return to the old one.
- **By date:** otherwise, for example after a backend-only deploy, which keeps the same release id. The last 3 days are compared with the 2 weeks before.

A slowdown becomes an issue (kind `perf`, one per page and metric, the worse device) only when all of these hold:
- each side has at least 50 measurements on that device;
- the rating gets worse (good → needs work → poor) and isn't good;
- the p75 grows by at least 25%, and by at least 500 ms for LCP, 100 ms for INP or 0.05 for CLS;
- the regression shows on two runs in a row.

Comparing each device on its own means a shift towards more phone visitors isn't mistaken for a slowdown.

**How a slowdown issue is handled:**

| Step | What happens |
|---|---|
| Explanation | From the measurements (`perf_regressions`): before and after, where the time goes, and what slow visits point at. The fix prompt has guardrails for each metric (for example, never lazy-load the main image) and marks the measurements as data. It doesn't use the error analyzer, the knowledge base or the model. |
| Severity | High when the page is now rated poor, otherwise medium. |
| Instant alerts | Only for poor slowdowns. The rest go to the daily summary, so they never use up the 3-a-day cap meant for errors. |
| Auto-fix | Always waits for the founder's approval, even with "start every high-impact fix without asking", because the agent can't measure speed. The auto-fix prompt is a speed-specific one. |
| Verification | By speed, not by events stopping. The issue is fixed once data from after its fix prompt (a newer release, or a date window that starts later) shows the page back at its earlier level. |
| Recurrence | If it slows down again, the same issue reopens for the next attempt. The agent's quick check answers "watching": speed needs real visits. |

The issue page shows the before and after, where the time goes and the culprit instead of error counts and the hourly chart. The issues list shows slow visits instead of people and times. Slowdowns don't count as setup's "first issue caught".

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

## Next: catching slowdowns from a release

Not built yet. The histograms keep the release, so the next step can compare a page's p75 in a new release with the one before, and raise an alert or an issue when it gets clearly worse.

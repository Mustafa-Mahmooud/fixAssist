# Phase 1 spike findings

2026-11-22 · Gate: **traces and Supabase bodies are usable** → **PASS, go to Phase 2**, with three design changes and two open risks below.

## Setup

- No Lovable access, so a Vite 6 + React 18 app (`spike/app`) stands in for a Lovable deploy: production build, esbuild minification, **no source maps**, one `index-<hash>.js` bundle. This is the same toolchain Lovable uses.
- Backend: a **hosted** Supabase project (West EU), because Docker can't run on the dev machine (Windows feature servicing is broken). The tables come from `spike/supabase/migrations/`.
- SDK: Sentry browser **8.55.0** CDN bundle (vendored) + our add-on (`pkg/sdk-addon/addon.js`).
- Harness (`spike/harness/run.mjs`, Playwright Chromium 138): build twice with different generated code, click 8 error cases per build, reload once, capture every envelope through `app spike-capture`.
- Golden files: 16 envelopes, 4 page views and `releases.json` in `internal/ingest/testdata/`. Gate tests: `go test ./internal/normalize -run Gate -v`.

## Gate results

| # | Criterion | Result |
|---|---|---|
| 1 | Go parses real envelopes from the pinned SDK | **Pass.** 16/16 envelopes, 16 events, every case in both builds. The SDK sends uncompressed `text/plain` envelopes with the key in `?sentry_key=`; no session items were seen. |
| 2 | JS fingerprint holds across redeploys | **Pass** for all 3 JS cases under both frame rules, after fix B below. The test is weaker than it looks; see risk 1. |
| 3 | Supabase codes extracted from 4xx bodies | **Pass**, after fix A below. 22001, 42501 (status **401** for anon), PGRST205 (404) and Auth `invalid_credentials` were all extracted. 5 network cases give 5 distinct, stable fingerprints. |
| 4 | Release hash changes per deploy, stable on reload | **Pass.** `fix-0bdd6fbc…` vs `fix-209cb257…`, the same on reload, and byte-identical when the same source is rebuilt. |
| – | Browser scrubbing | **Pass.** No emails, JWTs or passwords in any envelope. |

Signatures produced from real data:

```
net POST /rest/v1/profiles 400 pg:22001 "value too long for type character varying(20)" platform=lovable stack=supabase
net POST /rest/v1/secrets 401 pg:42501 "new row violates row-level security policy for table <str>" platform=lovable stack=supabase
net POST /auth/v1/token?grant_type 400 auth:invalid_credentials "Invalid login credentials" platform=lovable stack=supabase
net GET /rest/v1/missing_table?id&select 404 pg:PGRST205 "Could not find the table <str> in the schema cache" platform=lovable stack=supabase
net GET /ping 0 "Failed to fetch" platform=lovable stack=supabase
js TypeError "Cannot read properties of undefined (reading <str>)" route=/ platform=lovable stack=supabase
```

## What the spike changed

**A. Hosted GoTrue uses a body shape the FDR didn't list.** Hosted Supabase Auth returns `{"code":"invalid_credentials","message":"..."}`, with no `error_code` and a *string* `code`. That shape is too generic to identify from the body alone, so parsers now receive `bodyparse.Input{Status, URL, Body}`, and the Auth parser matches this shape only on `/auth/v1/` URLs. **FDR change:** `BodyParser` gets the request URL, not just the body.

**B. Browser scrubbing can't be allowed to change fingerprints.** A 13-digit order timestamp in `"Checkout failed for order 1795381381069"` matched the card regex and became `[card]` in one build but not the other, which split one issue in two. There are two fixes. First, the add-on now scrubs card-like numbers only when they pass the Luhn check. Second, and this one is load-bearing, `normalize.Message` maps scrub placeholders (`[card]`, `[email]`, `[jwt]`, `[token]`, `[secret]`) onto the same tokens as unscrubbed values. **Rule:** fingerprints must produce the same result whether or not the browser scrubbed a value.

**C. Release ID uses a synchronous hash, not SHA-256.** The add-on runs in `<head>` before the app's module scripts exist, and it needs a release on the very first event. `crypto.subtle` is async (and requires a secure context), so the add-on uses FNV-1a (64-bit). It computes the ID lazily on the first event, by which point module scripts are parsed, and caches it once `readyState !== "loading"`. Our own `data-fix-sdk` scripts are excluded, so upgrading the SDK isn't counted as a deploy. A collision only means one missed deploy.

Smaller notes:
- RLS denial for the anon role comes back as **401**, not 403. Network fingerprints include the status, so anon and logged-in RLS failures become two issues. That's probably acceptable, but check it in Phase 3 grouping review.
- Port 8080 was taken on the dev box, so the spike's capture server uses `127.0.0.1:8787`.

## Frame rules: which one to use

In a minified single-bundle app, **every crash-side frame has a 1–2 character name** (`lf`, `Hs`, `uo`, `tg`). So:

- **Rule A, drop-minified (FDR):** always drops all frames. The fingerprint is `type + message + route`.
- **Rule B, file-only:** contributes `/assets/index.js` ×3, which is the same for every error, so it adds nothing.

Both were stable and distinct here. **Keep rule A (the FDR rule) as the default.** Rule B adds no signal for Vite/Lovable bundles.

Function names that do survive minification are object-property names: `onClick`, `Object.run`, `Object.bd`. A possible rule C would keep the nearest non-minified, non-framework frame name. Worth evaluating in Phase 3 once partner data shows real merges.

## Open risks carried into Phase 2/3

1. **Grouping collisions on the same route (high).** Rule A drops all frames, so two different bugs that throw the same message on the same SPA route merge into one issue. "Cannot read properties of undefined (reading <str>)" on `/` is the worst case, made worse because quoted property names normalize to `<str>`. The spike couldn't show this because each case had a unique message. Mitigations to test in Phase 3: keep the quoted property name for TypeErrors (it's code, not data), add rule C, and check with the hand grouping review.
2. **Redeploy simulation was mild.** Build 2 changed only a separate generated module, so React's and the app's minified names were identical in both builds (`lf … Zv Xv` both times). Rule A doesn't depend on names, so the gate result holds. But we haven't seen what a real Lovable edit does to names and frame counts. Confirm on the first design-partner app.
3. **No real Lovable deploy yet.** Lovable's build may differ: chunk splitting, injected scripts, its own error overlay. That affects frames and the release hash. Check on the first partner install.
4. **SDK size.** Sentry 8.55.0 is **25.2 KB** gzipped plus the add-on at **2.4 KB** gzipped, 27.6 KB in total. The add-on is over the FDR's ~1 KB target because it also does the release ID, page views and Luhn scrubbing. The FDR's 3 KB Phase 6 budget still fits. Lazy-loading the SDK after first paint remains the fallback if founders object.
5. **Golden files contain the spike project's Supabase URL** (`kotivpouckqfgwmjlvcv.supabase.co`). It isn't secret, but if you'd rather not commit it, regenerate against a throwaway project.

## Go / no-go

**Go to Phase 2.** Envelopes parse, Supabase bodies give exact, stable codes for the errors that matter most, the release hash works with no setup, and scrubbing holds. Network failures are the strongest signal, as the FDR expected. JS grouping is usable but coarse; collision risk 1 is the main item for Phase 3's grouping review.

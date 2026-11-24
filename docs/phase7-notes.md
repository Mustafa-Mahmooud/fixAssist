# Phase 7: Cost and moat

2026-11-23 · **Built and verified locally**, adapted to agent mode. Billing was postponed (your decision).

The FDR's Phase 7 cuts LLM cost (a semantic cache, small and large model routing) and builds the moat (a confidence-scored knowledge base, GitHub code context). We run no LLM, so there's no model cost to cut. The moat became **cross-customer knowledge reuse** plus **file-level prompts**.

## Knowledge reuse (in place of the semantic cache)

- **One entry per error pattern across all apps.** The key is the issue signature without the per-app `platform=` and `stack=` tags (`kb_signature()` in SQL), so "value too long on `profiles`" from a Lovable app and a Cursor app share one track record. Existing entries were merged by migration 00006.
- **Apps per pattern** are counted in `kb_sightings` using hashed project IDs. Entries still hold no samples or customer data.
- **Track record on the issue page:** "This fix worked 4 of 5 times across 3 apps". When a pattern is **proven** (≥ 2 worked, confidence ≥ 60%), new issues get it with a **Proven fix** badge; the suggestion origin is `cache`, the FDR's term.
- **Similar errors** come from Postgres `pg_trgm` text similarity (≥ 0.5), with no AI vendor or embeddings. The page shows **totals only** ("similar errors in other apps were fixed 3 of 3 times"), never another app's signature, which could reveal its table names. A test checks this.
- **Embeddings** (`knowledge_entries.embedding`) stay unused. pgvector and an embedding provider can replace or augment trigram similarity later without schema changes.

## Confidence and automatic quarantine

- **Confidence** is (worked + 1) / (worked + failed + 2) per pattern (FDR). Every explicit and implicit answer updates **the pattern the suggestion actually used** (`fix_suggestions.kb_entry_id`).
- **Quarantine:** at ≥ 5 answers with confidence < 30%, the pattern is quarantined. New issues with that pattern get a **diagnose-in-your-code** prompt instead of the failing catalog fix ("The standard fix for this error didn't work for most apps that tried it…"). Answers to that fallback don't count for or against the quarantined pattern.
- **Admin** lists quarantined patterns with a **Reset** button (after rewriting the catalog entry), next to the per-catalog-entry success rates.

## GitHub App: file-level prompts

1. **Create the app (staff, once):** `/admin/github` → "Create the GitHub App on GitHub". It sends GitHub our manifest: **read-only** Contents and Metadata, the user is asked to sign in during the install, and no webhooks. GitHub calls back and we show the keys once, to copy into `.env` (`GITHUB_APP_ID`, `_SLUG`, `GITHUB_CLIENT_ID`, `_SECRET`, `GITHUB_APP_PRIVATE_KEY`).
2. **Connect a repo (founder):** "Connect your code" on the Setup page → install on GitHub → back at `/github/setup`. We check:
   - our signed state (it expires and is tied to this user and project);
   - that the project belongs to the signed-in org;
   - **GitHub's own answer that this user can access the installation.**

   Without that last check, anyone could attach someone else's installation ID and read their code. With one repo it's picked automatically; otherwise the founder chooses.
3. **Code context when explaining:**
   - `CodeTerms` picks precise searches per catalog entry:
     - the table, in `supabase.from('…')` calls and in `create table` migrations;
     - the property read in a null crash, or the `items.map` call;
     - the missing name, the failing URL, and the auth call involved.
   - We download the repo tarball (≤ 50 MB, cached 10 min) and scan it **in memory**, skipping `node_modules`, `dist` and minified files.
   - The prompt gains "Where to look in the code: `src/pages/Signup.tsx` line 2: `…`", and the references are stored with the suggestion.
   - The search runs **outside** the database transaction with a 20 s timeout, and any failure just means no code references.
   - Code itself is never stored or shared; only path, line and a ≤ 120-character snippet.

## Verified

- The **full suite** passes against Postgres. New tests cover:
  - cross-app reuse: two apps with different agents share one pattern, counted as 2 apps; after both say "worked", the third app gets the **proven** fix; similar-error totals; no per-app tags in keys;
  - quarantine: 5 failures quarantine the pattern; the next app gets the fallback prompt with no pattern link; its "worked" doesn't move the pattern;
  - the issue page renders the badge, track record and similar totals, and **another app's signature never appears**;
  - GitHub: the app JWT verifies with its public key; the scanner finds `supabase.from("profiles")` and the migration while skipping `node_modules` and `.min.js`; the manifest asks only for read access;
  - the full connect flow against a fake GitHub API: install redirect with signed state; **someone else's installation is refused (403)**; a tampered state is refused; the single repo is auto-selected; a real 22001 analysis then produces "Where to look in the code: `src/pages/Signup.tsx` line 2", with the references stored.
- **Golden set** 22/22.
- **Live:** migrations 6 and 7 ran on the dev database, merging the existing knowledge entries.

## Not done

- **Billing** (Stripe, plans, free tier): postponed by decision. Everyone stays on design-partner access.
- **A real GitHub App** hasn't been created yet. It needs a public URL for the callbacks, so it waits for the deployment; then it's one click on `/admin/github`.
- **Model routing:** nothing to route without an LLM. If one is switched on, `ai.LLM` (small and large tiers) and the cost logging are ready.
- **Tuning on real data:** proven ≥ 2 and ≥ 60%, quarantine ≥ 5 and < 30%, similarity ≥ 0.5.

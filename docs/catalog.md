# The error catalog

2026-11-25 · **97 entries** (up from 29). Each entry recognizes one kind of failure from the real wording of the error and quotes the exact line it matched. It then explains the failure in plain words and asks the founder's AI agent for one specific change, with a way to check it worked. Errors outside the catalog get the diagnose-and-fix prompt.

Code:
- [ai/catalog.go](../internal/ai/catalog.go): the original entries.
- [ai/server.go](../internal/ai/server.go): server database and runtime entries.
- [ai/catalog_more.go](../internal/ai/catalog_more.go): the extended catalog and the order everything is checked in.

Tests: [ai/catalog_more_test.go](../internal/ai/catalog_more_test.go) has 108 real error messages. Every entry must have at least one case.

## Entries

| Group | Entries | Covers |
| --- | --- | --- |
| Supabase database (`pg.*`) | 15 | value too long, row-level security, missing table, **missing column** (PGRST204), **missing function** (PGRST202), **missing or ambiguous relationship** (PGRST200/201), more than one row, duplicate, required value, foreign key, invalid input, **check rule**, **statement timeout**, **range past the end**, **expired session token** |
| Supabase setup, Edge Functions, Storage (`sb.*`) | 9 | **missing URL/key in production**, **wrong anon key**, **Edge Function CORS / not reachable**, **not deployed**, **failing** (missing secrets), **bucket missing**, **storage policies**, **file already exists**, **file too large** |
| Supabase Auth (`auth.*`) | 11 | wrong password, weak password, email not confirmed, user exists, email rate limit, **stale saved session** (refresh token), **expired or reused email links / PKCE**, **sign-up or provider disabled**, **invalid or unauthorized email (built-in SMTP)**, **same password**, **request rate limit** |
| Firebase (`fb.*`) | 8 | **missing config**, **security rules**, **missing Firestore index** (uses the error's own link), **wrong credentials**, **email in use**, **weak password**, **unauthorized domain**, **closed popup** |
| React and the browser (`js.*`) | 13 | reading a property of nothing, not a function, not defined, old code after a deploy, a response that isn't JSON, **hydration mismatch**, **render loop**, **hook order / invalid hook call**, **component outside its provider** (router, auth, query client), **object rendered as text**, **invalid date**, **infinite recursion**, **full localStorage** |
| Harmless noise (`noise.*`, low severity) | 3 | **cancelled requests (AbortError)**, **ResizeObserver loop**, **"Script error." from other domains** |
| Third-party APIs (`api.*`) | 4 | **invalid or missing API key** (OpenAI, Stripe, Anthropic, Google), **quota or credit exhausted**, **Stripe test vs live IDs**, **Stripe webhook signature**. Browser calls to paid APIs also get a warning that the secret key is exposed |
| Network (`net.*`) | 5 | request to a local address, unreachable service, **rate limited (429)**, **request too large (413)**, **gateway down or slow (502/503/504)** |
| Server, any language (`srv.*`) | 29 | number too big, text too long, duplicate, required value, foreign key, missing table or column, database unreachable, null reference, bad input format, **email/SMTP**, **database login**, **TLS certificate**, **missing setting or secret**, **JWT expired or invalid**, **deadlock**, **record not found / more than one**, **NULL in a non-nullable column**, **missing package or module**, **missing file**, **read-only disk**, **out of memory**, **invalid JSON body**, **body too large**, **CSRF token**, **EF Core tracking conflict**, **concurrency conflict**, **disk full**, **port in use**, **outbound timeout** |

New entries are in bold.

## How matching works

- **The facts each entry can match on:**
  - for failed requests: the request's status, backend and error code, message and body;
  - for page errors: the type and message;
  - for server errors: every message in the exception chain, root cause first.
- **Entries quote what they matched** (for a request, its message rather than the raw body) inside an opening that says where it happened: "My app's .NET (C#) server fails on the /Account/Register page with "SmtpException: Failure sending mail."". Server prompts also list the app's own stack lines.
- **The catalog is checked in order, and the first match wins.** The order is set in one place (`catalog_more.go`), so specific entries come before general ones. For example:
  - an expired session (`invalid_grant` + "Invalid Refresh Token") is matched before "wrong password";
  - SMTP on port 587 is matched before "connection refused" (database);
  - a .NET missing package (`FileNotFoundException: Could not load file or assembly`) is matched before "missing file";
  - Edge Function calls are matched before the generic unreachable-service entry;
  - OpenAI's quota error is matched before the generic 429;
  - a timeout on a database port (5432, 3306, 1433, ...) counts as the database being unreachable, other timeouts as an outbound timeout.

## Adding an entry

1. **Write it** in `catalog_more.go`, in the group whose position fits, with `mk(key, pattern, extra, build)` or a full `entry`. Quote the matched line, name the exact thing to change, and keep the ask to one change.
2. **Add at least one real error message per wording** (database, runtime or service) to `TestCatalogMore`. The test fails if an entry has no case, if text is broken (`%!`, an empty quote), or if any part of the suggestion is missing.
3. **Run the golden set:** `app eval`.
4. **After release,** the admin page shows each entry's success rate. An entry whose fix keeps failing is quarantined automatically (see [phase7-notes.md](phase7-notes.md)).

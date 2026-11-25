# Server-side errors

2026-11-25 · **Built and verified locally**, end to end with the official Node.js and Python SDKs over HTTPS.

The browser script only sees problems in visitors' browsers. A server error, such as a database refusing a save, reaches the browser as a finished error page, so it was invisible. Server errors now come from the app's backend itself. Any language works.

## How an app sends them

- The backend uses the **official Sentry SDK** for its language: Node.js, Python, .NET, PHP, Ruby, Java, Go, Elixir and others.
- It's pointed at the project's DSN, `https://<public key>@<ingest host>/<project id>`. The ingest endpoint already speaks the Sentry protocol, so there's no SDK of our own.
- The Setup page has a "Catch server errors too" card. It has a paste-into-your-agent prompt for any language, the DSN, and setups for Node, Python, .NET, PHP/Laravel, Rails, Spring Boot and Go.
  - Every setup sends errors only: tracing is off and personal data is off.
  - Each setup also suggests setting `SENTRY_RELEASE` to a value that changes on every deploy.

## What changed

**Ingest**
- An event is a **server error** (`kind = 'server'`) when its `platform` isn't `javascript` and it has an exception. Server SDKs send their language as the platform.
- Server SDKs send no `Origin` header, so the envelope and store endpoints now accept a missing Origin. Browsers always send one on these requests, so a project's domain list still applies to every browser. Page views still require an allowed Origin.
- Scrubbing now also drops data that server SDKs attach:
  - local variables in stack frames (`vars`);
  - `request.env` and `request.query_string`;
  - `modules`, `threads` and `debug_meta`.
- Releases are stored as `server:<release>`, so backend and frontend deploys never get mixed up.

**Grouping** ([normalize/server.go](../internal/normalize/server.go))
- The fingerprint combines four things:
  - the exception chain's types, root cause first;
  - the root cause's normalized message;
  - the app's own function nearest the crash, as module plus function, never a line number;
  - the route, or the transaction name for background jobs.
- Values and line numbers never split an issue.
- Library frames are never treated as the app's own code, even when the SDK says they are. The Node SDK on Windows marks `node_modules` frames as in-app.
- Function names are normalized across languages: .NET async state machines, V8 `[as …]` names, anonymous functions and return types.

**Fix messages** ([ai/server.go](../internal/ai/server.go))
- Nine catalog entries, each matching the wording of Postgres, SQL Server, MySQL/MariaDB, SQLite, Oracle, MongoDB and Prisma:
  - number out of range;
  - text too long;
  - duplicate;
  - required value missing;
  - foreign key;
  - missing table or column;
  - database unreachable;
  - null reference (in .NET, Java, Python, Node, Ruby, PHP and Go wording);
  - bad input format.
- Prompts name the server's language. Where it helps, they add the usual framework's fix: EF Core, Django and SQLAlchemy, Prisma, Laravel, Rails and JPA.
- Every server prompt ends with **"Where it happened"**: the app's own stack frames, with file, line, function and source line, crash site first.
  - This works without GitHub.
  - In .NET and Java, the root cause is often thrown inside the framework, so the frames come from the innermost exception that has app frames.
- Errors outside the catalog get a diagnose-and-fix prompt containing the full exception chain.

**Verification** ([verify/verify.go](../internal/verify/verify.go))
- Only a **server** release starts verifying a server fix. A frontend deploy doesn't.
- Page traffic carries the frontend's release, so a server fix's traffic and recurrences are counted from when the watch started.
- After "I applied the fix", a server issue doesn't wait for a release name to change, because many servers never change it.
- **Came back:** any occurrence after the watch started reopens the issue. The exception is an occurrence from an older server release while a newer one exists: that's an old instance still draining during a rolling deploy.

## Verified

- **Real SDKs.** Envelopes were captured from `@sentry/node` 8.55.0 (an Express app) and `sentry-sdk` 2.19.2 for Python (a background job against a real SQLite database). They're saved as fixtures in [ingest/testdata/server](../internal/ingest/testdata/server), with machine paths and names replaced.
- **.NET.** The .NET fixture (EF Core `DbUpdateException` wrapping `ArgumentException: Parameter value '7771.00' is out of range`) was written by hand in the Sentry .NET format, because no .NET SDK runs here.
- **Tests:**
  - every fixture gets the right catalog entry, a prompt naming the file and line, and a fully scrubbed payload;
  - 27 database and language messages are recognized;
  - the fingerprint ignores values and line numbers;
  - the full HTTP pipeline accepts server envelopes without an Origin header but still refuses foreign browsers and Origin-less page views;
  - verification and "came back" behave as described above, including rolling deploys and releases that never change.
- **Live.** Both SDKs sent their errors to the running server through the public HTTPS tunnel. That gave 8 events and 7 issues; two crashes on different order IDs merged into one. Each issue got the right explanation.
- The full suite passes, and the golden set is 22/22.

## Not done

- **Server performance and traces:** transactions are still acknowledged and dropped.
- **Mobile apps** (iOS, Android, Flutter) also send a non-JavaScript platform, so they'd be labelled "Server error". The grouping and prompts still work, but the label is wrong.
- **Users on server errors:** an issue counts people only when the backend sets a user ID. Without one, the issue shows its event count.

// Phase 4 gate: do the generated fix prompts actually fix the errors?
//
//   node harness/fixcheck.mjs before   original app + broken database
//   node harness/fixcheck.mjs after    fixed app copy (spike/fixcheck/app) + fixes/*.sql
//   node harness/fixcheck.mjs report   compare the two runs
//
// Database calls (/rest/v1) go to a local PostgREST on the spike_rest
// database (spike/fixcheck/postgrest.conf); auth calls (/auth/v1) go to the
// hosted Supabase project from spike/.env. Errors are captured by
// `app spike-capture` on 127.0.0.1:8788 into tmp/fixcheck/<phase>/.
import { spawn, spawnSync } from "node:child_process";
import { createServer } from "node:http";
import { readFileSync, writeFileSync, readdirSync, rmSync, existsSync, mkdirSync } from "node:fs";
import { join, extname, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { chromium } from "playwright";

const here = dirname(fileURLToPath(import.meta.url));
const spikeDir = join(here, "..");
const repo = join(spikeDir, "..");
const fixDir = join(spikeDir, "fixcheck");
const outDir = join(repo, "tmp", "fixcheck");
const PORT = 4173;
const CAPTURE = "http://127.0.0.1:8788";
const REST = "http://127.0.0.1:3010";
const PSQL = join(process.env.LOCALAPPDATA, "Programs", "pg16", "pgsql", "bin", "psql.exe");
const WAIT_MS = 5000;

// What "fixed" means per case. Default: the action no longer produces an
// error event. net_auth: wrong passwords are normal, so the fix is a clear
// message (the 400 itself is expected to remain).
const CASES = [
  "js_typeerror", "js_custom", "js_rejection", "net_22001", "net_42501", "net_auth", "net_404", "net_dead",
  "pg_unique", "pg_not_null", "pg_invalid_uuid", "pg_single", "auth_weak_password", "js_map", "js_json",
];
const TEXT_CHECKS = { net_auth: "Wrong email or password" };

function env() {
  const vals = {};
  for (const line of readFileSync(join(spikeDir, ".env"), "utf8").split(/\r?\n/)) {
    const m = line.match(/^\s*([A-Z_]+)\s*=\s*"?([^"]*)"?\s*$/);
    if (m) vals[m[1]] = m[2];
  }
  if (!vals.SUPABASE_URL || !vals.SUPABASE_ANON_KEY) throw new Error("spike/.env needs SUPABASE_URL and SUPABASE_ANON_KEY");
  return vals;
}

function sh(cmd, args, opts = {}) {
  const r = spawnSync(cmd, args, { encoding: "utf8", shell: opts.shell ?? false, cwd: opts.cwd });
  if (r.status !== 0) throw new Error(`${cmd} ${args.join(" ")} failed:\n${r.stdout}\n${r.stderr}`);
  return r.stdout;
}

function psqlFile(file) {
  sh(PSQL, ["-h", "127.0.0.1", "-p", "5433", "-U", "fix", "-d", "spike_rest", "-q", "-v", "ON_ERROR_STOP=1", "-f", file]);
}

const TYPES = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".json": "application/json" };

// Static app + same-origin proxy, like a deployed Supabase app.
function serve(dist, supabaseURL) {
  const srv = createServer(async (req, res) => {
    const url = new URL(req.url, "http://x");
    if (url.pathname === "/ping") {
      res.writeHead(200, { "Content-Type": "text/plain" });
      return res.end("pong");
    }
    const rest = url.pathname.startsWith("/rest/v1/");
    const auth = url.pathname.startsWith("/auth/v1/");
    if (rest || auth) {
      const chunks = [];
      for await (const c of req) chunks.push(c);
      const headers = {};
      for (const [k, v] of Object.entries(req.headers)) {
        if (["host", "connection", "content-length", "origin", "referer", "accept-encoding"].includes(k)) continue;
        if (rest && (k === "authorization" || k === "apikey")) continue; // local PostgREST: anon role
        headers[k] = v;
      }
      const target = rest ? REST + url.pathname.slice("/rest/v1".length) + url.search : supabaseURL + url.pathname + url.search;
      try {
        const r = await fetch(target, { method: req.method, headers, body: ["GET", "HEAD"].includes(req.method) ? undefined : Buffer.concat(chunks) });
        const body = Buffer.from(await r.arrayBuffer());
        const out = {};
        r.headers.forEach((v, k) => { if (!["content-encoding", "transfer-encoding", "connection"].includes(k)) out[k] = v; });
        res.writeHead(r.status, out);
        return res.end(body);
      } catch (e) {
        res.writeHead(502);
        return res.end(String(e));
      }
    }
    let file = join(dist, decodeURIComponent(url.pathname));
    if (!file.startsWith(dist) || !existsSync(file) || url.pathname.endsWith("/")) file = join(dist, "index.html");
    res.writeHead(200, { "Content-Type": TYPES[extname(file)] || "application/octet-stream", "Cache-Control": "no-store" });
    res.end(readFileSync(file));
  });
  return new Promise((ok) => srv.listen(PORT, () => ok(srv)));
}

async function waitHTTP(url, what) {
  for (let i = 0; i < 40; i++) {
    try {
      await fetch(url);
      return;
    } catch {
      await new Promise((r) => setTimeout(r, 250));
    }
  }
  throw new Error(`${what} not reachable at ${url}`);
}

async function run(phase) {
  const e = env();
  const appDir = phase === "before" ? join(spikeDir, "app") : join(fixDir, "app");
  if (!existsSync(appDir)) throw new Error(`${appDir} doesn't exist yet`);
  await waitHTTP(REST + "/", "PostgREST (start it with spike/fixcheck/postgrest.conf)");

  // Database: broken schema, plus the fix migrations for "after".
  psqlFile(join(fixDir, "db", "schema.sql"));
  if (phase === "after") {
    const fixes = existsSync(join(fixDir, "fixes")) ? readdirSync(join(fixDir, "fixes")).filter((f) => f.endsWith(".sql")).sort() : [];
    for (const f of fixes) psqlFile(join(fixDir, "fixes", f));
    console.log("applied fixes:", fixes.join(", ") || "(none)");
  }
  await new Promise((r) => setTimeout(r, 1000)); // PostgREST schema reload

  const capDir = join(outDir, phase, "envelopes");
  rmSync(join(outDir, phase), { recursive: true, force: true });
  mkdirSync(capDir, { recursive: true });
  const cap = spawn(join(repo, "bin", "app.exe"), ["spike-capture", "--out", capDir, "--addr", "127.0.0.1:8788"], { stdio: "ignore" });
  try {
    await waitHTTP(CAPTURE + "/healthz", "capture server");
    writeFileSync(join(appDir, ".env.local"),
      `VITE_SUPABASE_URL=http://localhost:${PORT}\nVITE_SUPABASE_ANON_KEY=${e.SUPABASE_ANON_KEY}\n` +
      `VITE_FIX_SDK_URL=${CAPTURE}/sdk/spikekey.js\nVITE_PING_URL=http://localhost:${PORT}/ping\n`);
    writeFileSync(join(appDir, "src", "buildinfo.js"), "export const BUILD = 1;\nexport const extras = [];\n");
    sh("npm", ["run", "build"], { cwd: appDir, shell: true });
    const srv = await serve(join(appDir, "dist"), e.SUPABASE_URL.replace(/\/+$/, ""));

    const browser = await chromium.launch();
    const page = await browser.newPage();
    await page.goto(`http://localhost:${PORT}/`, { waitUntil: "load" });
    const results = {};
    for (const c of CASES) {
      const resp = page.waitForResponse((r) => r.url().includes("/envelope/"), { timeout: WAIT_MS }).catch(() => null);
      const button = page.locator(`[data-case=${c}]`);
      if (!(await button.count())) {
        results[c] = { missing: true };
        continue;
      }
      await button.click();
      const r = await resp;
      await page.waitForTimeout(300);
      const want = TEXT_CHECKS[c];
      const text = want ? await page.locator(`text=${want}`).count() > 0 : null;
      results[c] = { error_event: !!r, text };
      console.log(`  ${c.padEnd(20)} error event: ${r ? "yes" : "no "}${want ? `   message shown: ${text ? "yes" : "no"}` : ""}`);
    }
    await browser.close();
    await new Promise((ok) => srv.close(ok));
    writeFileSync(join(outDir, `${phase}.json`), JSON.stringify(results, null, 2) + "\n");
    console.log(`wrote tmp/fixcheck/${phase}.json`);
  } finally {
    cap.kill();
  }
}

function broken(r, c) {
  if (!r || r.missing) return true;
  if (TEXT_CHECKS[c]) return !r.text;
  return r.error_event;
}

function report() {
  const before = JSON.parse(readFileSync(join(outDir, "before.json"), "utf8"));
  const after = JSON.parse(readFileSync(join(outDir, "after.json"), "utf8"));
  let fixed = 0, reproduced = 0;
  console.log("CASE                  BEFORE   AFTER");
  for (const c of CASES) {
    const b = broken(before[c], c), a = broken(after[c], c);
    if (b) reproduced++;
    if (b && !a) fixed++;
    console.log(`${c.padEnd(22)}${(b ? "broken" : "ok").padEnd(9)}${!b ? "(not reproduced)" : a ? "broken" : "FIXED"}`);
  }
  console.log(`\n${fixed}/${reproduced} reproduced cases fixed by following the generated prompts`);
}

const phase = process.argv[2];
if (phase === "report") report();
else if (phase === "before" || phase === "after") await run(phase);
else throw new Error("usage: fixcheck.mjs before|after|report");

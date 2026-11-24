// Spike harness (Phase 1 golden files, Phase 2 gate).
//
// Prereqs: a Supabase backend (spike/.env), the add-on built, and either
//   capture mode: `app spike-capture` running (writes golden envelopes), or
//   db mode:      `app serve` running and FIX_PUBLIC_KEY set (writes Postgres).
//
// For build 1 and build 2: regenerate src/buildinfo.js (so the bundle changes
// like a real redeploy), `vite build`, serve dist/, open it in headless Chromium,
// click every error case, reload once, and record release IDs and statuses.
import { spawnSync } from "node:child_process";
import { createServer } from "node:http";
import { readFileSync, writeFileSync, readdirSync, rmSync, existsSync, mkdirSync } from "node:fs";
import { join, extname, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { chromium } from "playwright";

const here = dirname(fileURLToPath(import.meta.url));
const spikeDir = join(here, "..");
const appDir = join(spikeDir, "app");
const repo = join(spikeDir, "..");
const testdata = join(repo, "internal", "ingest", "testdata");
const envelopesDir = join(testdata, "envelopes");
const INGEST = process.env.INGEST_URL || "http://127.0.0.1:8787";
const PORT = Number(process.env.APP_PORT || 4173);
const CASE_TIMEOUT_MS = 15000;
const CASE_SETTLE_MS = 300;
// capture: write golden files via `app spike-capture` (any key works).
// db: run against `app serve` with a real project key (FIX_PUBLIC_KEY).
const MODE = process.env.HARNESS_MODE || "capture";
const PUBLIC_KEY = process.env.FIX_PUBLIC_KEY || "spikekey";
if (MODE === "db" && !process.env.FIX_PUBLIC_KEY) throw new Error("HARNESS_MODE=db needs FIX_PUBLIC_KEY");

function sh(cmd, cwd) {
  const r = spawnSync(cmd, { cwd, shell: true, encoding: "utf8" });
  if (r.status !== 0) {
    console.error(r.stdout, r.stderr);
    throw new Error(`failed: ${cmd}`);
  }
  return r.stdout;
}

async function checkIngest() {
  try {
    const r = await fetch(INGEST + "/healthz");
    if (!r.ok) throw new Error(String(r.status));
  } catch (e) {
    throw new Error(`capture server not reachable at ${INGEST}: run \`go run ./cmd/app spike-capture\` from the repo root (${e.message})`);
  }
}

// Hosted project: SUPABASE_URL / SUPABASE_ANON_KEY from the environment or
// from spike/.env (gitignored). Otherwise fall back to the local CLI.
function supabaseEnv() {
  const envFile = join(spikeDir, ".env");
  if (existsSync(envFile)) {
    for (const line of readFileSync(envFile, "utf8").split(/\r?\n/)) {
      const m = line.match(/^\s*([A-Z_]+)\s*=\s*"?([^"]*)"?\s*$/);
      if (m && !process.env[m[1]]) process.env[m[1]] = m[2];
    }
  }
  if (process.env.SUPABASE_URL && process.env.SUPABASE_ANON_KEY) {
    return { url: process.env.SUPABASE_URL.replace(/\/+$/, ""), key: process.env.SUPABASE_ANON_KEY };
  }
  const out = sh("npx supabase status -o env", spikeDir);
  const get = (k) => (out.match(new RegExp(`^${k}="?([^"\\r\\n]+)"?`, "m")) || [])[1];
  const url = get("API_URL"), key = get("ANON_KEY");
  if (!url || !key) throw new Error("could not read API_URL / ANON_KEY from `supabase status`; is it running?");
  return { url, key };
}

// Each build gets a different amount of generated code, which shifts the
// minifier's identifier assignment and every line/column, like a real edit.
function writeBuildInfo(build) {
  const n = build * 6;
  const fns = Array.from({ length: n }, (_, i) => `  (x) => (x * ${i + build} + ${build * 7}) % ${97 + i}`);
  writeFileSync(join(appDir, "src", "buildinfo.js"), `export const BUILD = ${build};\nexport const extras = [\n${fns.join(",\n")}\n];\n`);
}

const TYPES = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".json": "application/json" };

function serve(dir) {
  const srv = createServer((req, res) => {
    let p = decodeURIComponent(new URL(req.url, "http://x").pathname);
    let file = join(dir, p);
    if (!file.startsWith(dir) || !existsSync(file) || p.endsWith("/")) file = join(dir, "index.html");
    res.writeHead(200, { "Content-Type": TYPES[extname(file)] || "application/octet-stream", "Cache-Control": "no-store" });
    res.end(readFileSync(file));
  });
  return new Promise((ok) => srv.listen(PORT, () => ok(srv)));
}

function clearCaptures() {
  mkdirSync(envelopesDir, { recursive: true });
  for (const f of readdirSync(envelopesDir)) {
    if (/\.(envelope|meta\.json|pv\.json)$/.test(f)) rmSync(join(envelopesDir, f));
  }
}

async function main() {
  await checkIngest();
  const sb = supabaseEnv();
  const sdkUrl = `${INGEST}/sdk/${PUBLIC_KEY}.js`;
  writeFileSync(join(appDir, ".env.local"), `VITE_SUPABASE_URL=${sb.url}\nVITE_SUPABASE_ANON_KEY=${sb.key}\nVITE_FIX_SDK_URL=${sdkUrl}\n`);
  console.log(`mode=${MODE} sdk=${sdkUrl}`);
  if (MODE === "capture") clearCaptures();

  const browser = await chromium.launch();
  const result = { generated_at: new Date().toISOString(), mode: MODE, builds: [] };
  const cases = ["js_typeerror", "js_custom", "js_rejection", "net_22001", "net_42501", "net_auth", "net_404", "net_dead"];
  let missing = 0;

  for (const build of [1, 2]) {
    console.log(`\n== build ${build}`);
    writeBuildInfo(build);
    sh("npm run build", appDir);
    const assets = readdirSync(join(appDir, "dist", "assets"));
    console.log("assets:", assets.join(", "));
    const srv = await serve(join(appDir, "dist"));

    const page = await browser.newPage();
    const pvStatuses = [];
    page.on("response", (r) => { if (r.url().includes("/v1/pv")) pvStatuses.push(r.status()); });
    await page.goto(`http://localhost:${PORT}/`, { waitUntil: "load" });
    const loads = [await page.evaluate(() => window.__fix && window.__fix.release())];
    const statuses = {};
    for (const c of cases) {
      // Wait for this case's envelope response before the next click, so a slow
      // backend can't leak the next case's spike_case tag onto this event.
      const start = Date.now();
      const resp = page.waitForResponse((r) => r.url().includes("/envelope/"), { timeout: CASE_TIMEOUT_MS }).catch(() => null);
      await page.click(`[data-case=${c}]`);
      const r = await resp;
      statuses[c] = r ? r.status() : null;
      if (!r) missing++;
      console.log(`  case ${c}: ${r ? `HTTP ${r.status()} after ${Date.now() - start} ms` : "NO ENVELOPE (timeout)"}`);
      await page.waitForTimeout(CASE_SETTLE_MS);
    }
    await page.reload({ waitUntil: "load" });
    loads.push(await page.evaluate(() => window.__fix && window.__fix.release()));
    await page.waitForTimeout(2000); // let the page-view beacon flush
    await page.close();
    await new Promise((ok) => srv.close(ok));

    console.log("  releases:", loads.join(" "), " pageview statuses:", pvStatuses.join(","));
    result.builds.push({ build, assets, loads, statuses, pageview_statuses: pvStatuses });
  }
  await browser.close();

  if (MODE === "capture") {
    writeFileSync(join(testdata, "releases.json"), JSON.stringify(result, null, 2) + "\n");
    const n = readdirSync(envelopesDir).filter((f) => f.endsWith(".envelope")).length;
    console.log(`\nwrote releases.json; ${n} envelopes captured in ${envelopesDir}`);
  } else {
    writeFileSync(join(here, "last-run.json"), JSON.stringify(result, null, 2) + "\n");
    console.log(`\nwrote harness/last-run.json`);
  }
  if (missing) throw new Error(`${missing} cases produced no envelope`);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});

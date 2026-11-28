// Manual check that the built SDK (pkg/sdk-addon/dist) reports Core Web
// Vitals from a real Chrome, attributed, with query values dropped and no
// page errors. Run after changing the add-on or upgrading web-vitals:
//   cd spike && node harness/vitals-check.mjs
import http from "node:http";
import { readFileSync } from "node:fs";
import zlib from "node:zlib";
import { randomBytes } from "node:crypto";
import { chromium } from "playwright";

const root = new URL("../../pkg/sdk-addon/", import.meta.url);
const sentry = readFileSync(new URL("vendor/sentry-8.55.0.bundle.min.js", root));
const addon = readFileSync(new URL("dist/addon.min.js", root));
// A real 900x500 image: Chrome sizes an image's LCP by its natural size.
function makePNG(w, h) {
  const crcTable = Array.from({ length: 256 }, (_, n) => { let c = n; for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1; return c >>> 0; });
  const crc = (b) => { let c = 0xffffffff; for (const x of b) c = crcTable[(c ^ x) & 0xff] ^ (c >>> 8); return (c ^ 0xffffffff) >>> 0; };
  const chunk = (type, data) => { const len = Buffer.alloc(4); len.writeUInt32BE(data.length); const td = Buffer.concat([Buffer.from(type), data]); const c = Buffer.alloc(4); c.writeUInt32BE(crc(td)); return Buffer.concat([len, td, c]); };
  const ihdr = Buffer.alloc(13); ihdr.writeUInt32BE(w, 0); ihdr.writeUInt32BE(h, 4); ihdr[8] = 8; ihdr[9] = 2;
  const raw = randomBytes((w * 3 + 1) * h); // photo-like: Chrome skips low-information images for (let y = 0; y < h; y++) raw[y * (w * 3 + 1)] = 0;
  return Buffer.concat([Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]), chunk("IHDR", ihdr), chunk("IDAT", zlib.deflateSync(raw)), chunk("IEND", Buffer.alloc(0))]);
}
const png = makePNG(900, 500);
const got = [];

const srv = http.createServer((req, res) => {
  const u = new URL(req.url, "http://x");
  if (req.method === "POST") {
    let b = "";
    req.on("data", (c) => (b += c));
    req.on("end", () => {
      if (u.pathname === "/v1/a") for (const e of JSON.parse(b).events) if (e.type === "vital") got.push(e);
      res.writeHead(204, { "Access-Control-Allow-Origin": "*" }).end();
    });
    return;
  }
  if (u.pathname === "/sentry.js") return res.writeHead(200, { "Content-Type": "text/javascript" }).end(sentry);
  if (u.pathname === "/addon.js") return res.writeHead(200, { "Content-Type": "text/javascript" }).end(addon);
  if (u.pathname === "/hero.png") return setTimeout(() => res.writeHead(200, { "Content-Type": "image/png" }).end(png), 400);
  const port = srv.address().port;
  res.writeHead(200, { "Content-Type": "text/html" }).end(`<!doctype html><html><body style="margin:0">
<script>window.__FIX_CONFIG = { dsn: "http://pubkey@127.0.0.1:${port}/1" };</script>
<script src="/sentry.js"></script><script src="/addon.js"></script>
<div id="slot"></div>
<h1>Pricing</h1>
<img class="hero" src="/hero.png?token=SECRET123" style="width:900px;height:500px;display:block">
<button id="buy" onclick="var t=Date.now();while(Date.now()-t<300){}">Buy</button>
<script>setTimeout(function(){document.getElementById("slot").innerHTML='<div style="height:200px;background:#eee">Late banner</div>'},700)</script>
</body></html>`);
});
await new Promise((r) => srv.listen(0, "127.0.0.1", r));
const port = srv.address().port;

const browser = await chromium.launch();
const page = await browser.newPage();
const errors = [];
page.on("pageerror", (e) => errors.push(String(e)));
await page.goto(`http://127.0.0.1:${port}/pricing`, { waitUntil: "load" });
await page.waitForTimeout(1500);
await page.click("#buy");
await page.waitForTimeout(500);
// The visitor switches tabs: headless tabs never become hidden by themselves,
// so say so the way the browser would. CLS, INP and LCP are reported now.
await page.evaluate(() => {
  Object.defineProperty(document, "visibilityState", { value: "hidden", configurable: true });
  document.dispatchEvent(new Event("visibilitychange"));
});
await page.waitForTimeout(1500);
await page.close();
await browser.close();
srv.close();

const by = Object.fromEntries(got.map((e) => [e.name, e]));
for (const e of got) console.log(e.name.padEnd(5), JSON.stringify({ path: e.path, ...e.props }));
const fail = [];
for (const m of ["LCP", "INP", "CLS", "FCP", "TTFB"]) if (!by[m]) fail.push(`no ${m}`);
if (by.LCP && !(by.LCP.props.el || "").includes("img.hero")) fail.push("LCP element isn't the hero image");
if (by.LCP && (by.LCP.props.src || "").includes("SECRET123")) fail.push("LCP URL kept a query value");
if (by.INP && !(by.INP.props.el || "").includes("#buy")) fail.push("INP target isn't the button");
if (by.INP && by.INP.props.v < 250) fail.push(`INP ${by.INP.props.v} ms, the handler blocked 300 ms`);
if (by.CLS && !(by.CLS.props.v > 0)) fail.push("no layout shift measured");
if (got.some((e) => e.path !== "/pricing")) fail.push("wrong path");
if (errors.length) fail.push("page errors: " + errors.join("; "));
console.log(fail.length ? "FAIL: " + fail.join(", ") : "OK: all five vitals, attributed, no query values, no errors");
process.exit(fail.length ? 1 : 0);

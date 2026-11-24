// Prints raw and gzipped sizes of the add-on and the pinned Sentry bundle.
import { readFileSync } from "node:fs";
import { gzipSync } from "node:zlib";

const files = ["dist/addon.min.js", "vendor/sentry-8.55.0.bundle.min.js"];
let total = 0;
for (const f of files) {
  const buf = readFileSync(new URL(f, import.meta.url));
  const gz = gzipSync(buf, { level: 9 }).length;
  total += gz;
  console.log(`${f.padEnd(40)} ${String(buf.length).padStart(7)} B  ${String(gz).padStart(6)} B gzip`);
}
console.log(`${"total (gzip)".padEnd(40)} ${String(total).padStart(16)} B`);

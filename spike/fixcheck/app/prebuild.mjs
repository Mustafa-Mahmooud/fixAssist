// Writes a default build marker for dev runs; the harness overwrites it per build.
import { existsSync, writeFileSync } from "node:fs";
const buildinfo = new URL("./src/buildinfo.js", import.meta.url);
if (!existsSync(buildinfo)) writeFileSync(buildinfo, "export const BUILD = 0;\nexport const extras = [];\n");

import React from "react";
import { cases } from "./cases.js";
import { BUILD, extras } from "./buildinfo.js";

function runCase(c) {
  // Tag every event from this click so the Go gate tests can match cases.
  window.Sentry && window.Sentry.setTag("spike_case", c.id);
  c.run();
}

export function App() {
  // Use the generated extras so they are not tree-shaken; they shift the
  // minifier's naming between builds, like a real redeploy.
  const checksum = extras.reduce((acc, f) => acc + f(acc), 0);
  return (
    <main style={{ fontFamily: "system-ui", padding: 24, maxWidth: 520 }}>
      <h1>Fix spike app</h1>
      <p>
        Build {BUILD} <span style={{ color: "#888" }}>({checksum})</span>
      </p>
      {cases.map((c) => (
        <p key={c.id}>
          <button data-case={c.id} onClick={() => runCase(c)}>
            {c.label}
          </button>
        </p>
      ))}
    </main>
  );
}

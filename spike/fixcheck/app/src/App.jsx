import React, { useEffect, useState } from "react";
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
  // Fixed copy: shows messages from showMessage() (net_auth, net_42501, ...).
  const [message, setMessage] = useState(null);
  useEffect(() => {
    const on = (e) => setMessage(e.detail);
    window.addEventListener("app-message", on);
    return () => window.removeEventListener("app-message", on);
  }, []);
  return (
    <main style={{ fontFamily: "system-ui", padding: 24, maxWidth: 520 }}>
      <h1>Fix spike app</h1>
      <p>
        Build {BUILD} <span style={{ color: "#888" }}>({checksum})</span>
      </p>
      {message && (
        <p role="status" style={{ padding: 8, border: "1px solid #ccc" }}>
          {message.text}{" "}
          {message.action && <a href="#" onClick={(e) => { e.preventDefault(); message.action.onClick(); }}>{message.action.label}</a>}
        </p>
      )}
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

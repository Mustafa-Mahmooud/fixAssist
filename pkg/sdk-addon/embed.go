// Package sdkaddon embeds the browser bundle served at /sdk/{public_key}.js:
// the pinned Sentry SDK, a config line with the project's DSN, and the add-on.
// Rebuild dist/addon.min.js with `npm run build` after editing addon.js.
package sdkaddon

import (
	_ "embed"
	"encoding/json"
)

// SentryVersion is the pinned Sentry browser SDK.
const SentryVersion = "8.55.0"

//go:embed vendor/sentry-8.55.0.bundle.min.js
var sentryBundle []byte

//go:embed dist/addon.min.js
var addon []byte

// Bundle returns the full script for one DSN.
func Bundle(dsn, environment string) []byte {
	cfg, _ := json.Marshal(map[string]string{"dsn": dsn, "environment": environment})
	out := make([]byte, 0, len(sentryBundle)+len(addon)+len(cfg)+64)
	out = append(out, sentryBundle...)
	out = append(out, "\n;window.__FIX_CONFIG="...)
	out = append(out, cfg...)
	out = append(out, ";\n"...)
	out = append(out, addon...)
	return out
}

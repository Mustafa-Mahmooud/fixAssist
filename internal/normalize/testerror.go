package normalize

import "strings"

// TestErrorMessage is the error a founder sends on purpose to check their
// setup: the add-on throws it when the page URL has ?fixassist-test, and the
// Setup page shows the same line for the browser console.
const TestErrorMessage = "Fix Assistant test"

// IsTestError reports whether a JS error is the setup test error.
func IsTestError(errType, message string) bool {
	return (errType == "" || errType == "Error") && strings.TrimSpace(message) == TestErrorMessage
}

// TestErrorFingerprint is one fingerprint for every test error, whatever the
// page or stack: anyone can open a site with ?fixassist-test, so test errors
// must never turn into one issue per path.
func TestErrorFingerprint() string { return hash(Version + "|js|setup-test") }

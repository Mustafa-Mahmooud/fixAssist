package web

import "testing"

// Templates are parsed in init(), so a missing template function would only
// show up when the server starts. Importing the package in a test catches it
// in CI; this test also checks every page parsed.
func TestAllPagesParse(t *testing.T) {
	for _, p := range []string{"login", "magic", "projects", "setup", "issues", "issue", "alerts", "feedback", "admin", "overview", "pages", "funnels", "insights", "admin_github", "partials"} {
		if pages[p] == nil {
			t.Errorf("page %q not parsed", p)
		}
	}
}

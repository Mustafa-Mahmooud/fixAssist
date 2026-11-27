package alerts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderHTML(t *testing.T) {
	e := Email{
		Preheader: "Checkout is failing",
		Label:     "New issue",
		Tone:      "bad",
		Heading:   "Checkout button <crashes>",
		Paras:     []string{"The checkout page throws when the cart is empty.", "3 people hit it today."},
		Items:     []Item{{Title: "TypeError in Cart", Meta: "3 people affected, 9 times", URL: "https://app.example/projects/1/issues/2", Badge: "high"}},
		Stats:     []Stat{{"New issues", "1"}, {"Still open", "4"}},
		CodeLabel: "Paste this into Lovable:",
		Code:      "Fix the cart:\n  if (!cart) return <Empty/>",
		Buttons:   []Button{{"Open the issue", "https://app.example/projects/1/issues/2"}, {"Bad", "javascript:alert(1)"}},
		Note:      "Tried the fix?",
	}
	got := RenderHTML(e)
	for _, want := range []string{
		"Checkout button &lt;crashes&gt;",                // escaped
		"if (!cart) return &lt;Empty/&gt;",               // code escaped
		`href="https://app.example/projects/1/issues/2"`, // real link kept
		"#c62828;background:#fdecec",                     // tone colour applied
		"Checkout is failing",                            // preheader
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(got, "javascript:") {
		t.Error("unsafe URL was not filtered")
	}
	if strings.Contains(got, "ZgotmplZ") && !strings.Contains(got, `href="#ZgotmplZ"`) {
		t.Error("template escaping broke a CSS value")
	}
	if dir := os.Getenv("EMAIL_PREVIEW_DIR"); dir != "" {
		_ = os.WriteFile(filepath.Join(dir, "preview.html"), []byte(got), 0o644)
	}
}

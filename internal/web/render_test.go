package web

import "testing"

func TestCodeText(t *testing.T) {
	got := string(codeText("Change `profiles.email` to <text> & `x`"))
	want := "Change <code>profiles.email</code> to &lt;text&gt; &amp; <code>x</code>"
	if got != want {
		t.Errorf("codeText = %q, want %q", got, want)
	}
}

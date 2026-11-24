package normalize

import (
	"strings"
	"testing"
)

func TestMessageV2(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Cannot read properties of undefined (reading 'profile')", "Cannot read properties of undefined (reading 'profile')"},
		{"undefined is not an object (evaluating 'user.profile.name')", "undefined is not an object (evaluating 'user.profile.name')"},
		{`can't access property "profile", user is undefined`, `can't access property "profile", user is undefined`},
		{"'onSave' is not a function", "'onSave' is not a function"},
		{"formatPrice is not defined", "formatPrice is not defined"},
		// Quoted user data is still placeholdered.
		{"Checkout failed for 'jane doe' order 1795381381069", "Checkout failed for <str> order <n>"},
		{"relation \"public.missing\" does not exist", "relation <str> does not exist"},
		// Non-identifier text in an engine-looking quote is not kept.
		{"Cannot read properties of undefined (reading 'a b')", "Cannot read properties of undefined (reading <str>)"},
	}
	for _, c := range cases {
		if got := MessageV2(c.in); got != c.want {
			t.Errorf("MessageV2(%q)\n got  %q\n want %q", c.in, got, c.want)
		}
	}
}

func TestJSFingerprintV2SeparatesWhatV1Merged(t *testing.T) {
	page := "https://app.example.com/"
	frames := func(names ...string) []Frame {
		var f []Frame
		for _, n := range names {
			f = append(f, Frame{Filename: "https://app.example.com/assets/index-DpRClB3D.js", Function: n})
		}
		return f
	}
	a := JSError{Type: "TypeError", Message: "Cannot read properties of undefined (reading 'profile')", PageURL: page, Frames: frames("lf", "onClick", "Xv")}
	b := JSError{Type: "TypeError", Message: "Cannot read properties of undefined (reading 'items')", PageURL: page, Frames: frames("lf", "onClick", "Qa")}
	if JSFingerprint(a, RuleDropMinified) != JSFingerprint(b, RuleDropMinified) {
		t.Fatal("precondition: v1 should merge these")
	}
	if JSFingerprintV2(a) == JSFingerprintV2(b) {
		t.Error("v2 should separate different properties")
	}
	c := a
	c.Frames = frames("lf", "handleSave", "Xv")
	if JSFingerprintV2(a) == JSFingerprintV2(c) {
		t.Error("v2 should separate different named handlers")
	}
	// Minified names change between builds; v2 must not care.
	d := a
	d.Frames = frames("Zq", "onClick", "Pm")
	if JSFingerprintV2(a) != JSFingerprintV2(d) {
		t.Error("v2 changed with minified names")
	}
	if !strings.HasPrefix(JSFingerprintV2(a), "v2:") {
		t.Error("missing v2 prefix")
	}
	if got := NearestNamedFrame(JSError{PageURL: page, Frames: frames("onClick", "dispatchEvent", "Object.run", "Xv")}); got != "run" {
		t.Errorf("NearestNamedFrame = %q, want run (framework names skipped)", got)
	}
}

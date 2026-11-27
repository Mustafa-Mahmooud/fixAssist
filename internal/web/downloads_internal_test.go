package web

import (
	"strings"
	"testing"
)

func TestRenderScriptPairCode(t *testing.T) {
	got := renderScript(installPS1, "https://fix.example/", "k7qm4x2pra")
	if !strings.Contains(got, "$pair = 'K7QM4-X2PRA'") || !strings.Contains(got, "pair --server 'https://fix.example'") {
		t.Errorf("pair code not filled in:\n%s", got)
	}
	// Only code characters get through: nothing can break out of the quotes.
	for _, evil := range []string{"'; rm -rf ~; '", "AAAAA-AAAAA$(id)", "x"} {
		got := renderScript(installSH, "https://fix.example", evil)
		if strings.Contains(got, "rm -rf") || strings.Contains(got, "$(id)") || !strings.Contains(got, "pair=''") {
			t.Errorf("pair=%q rendered unsafely", evil)
		}
	}
}

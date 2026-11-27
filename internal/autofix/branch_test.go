package autofix

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The same cases as cmd/fixassist's safeBranch: the server and the daemon
// must agree on which names reach git.
func TestValidBranch(t *testing.T) {
	for _, ok := range []string{"main", "master", "dev", "feature/learning-errors", "release-1.4", "v2+fix", "user_x/y"} {
		if !ValidBranch(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "-x", "--upload-pack=touch /tmp/p", "/main", ".hidden", "a..b", "a//b", "a/.b", "a@{1}", "a b", "a;b",
		"a$(x)", "main/", "main.", "x.lock", "a\nb", "a\\b", "a:b", "a~1", "a^", "a?", "a*", "a[", strings.Repeat("a", 201)} {
		if ValidBranch(bad) {
			t.Errorf("%q allowed", bad)
		}
	}
}

// A hint in Arabic (two bytes a letter) is cut between letters, never inside
// one: Postgres refuses half a character.
func TestClipKeepsCharacters(t *testing.T) {
	s := strings.Repeat("الكود في المجلد", 100)
	for _, n := range []int{1, 2, 3, MaxHint, MaxHint + 1} {
		if c := clip(s, n); !utf8.ValidString(c) || len(c) > n {
			t.Fatalf("clip(%d) = %d bytes, valid %v", n, len(c), utf8.ValidString(c))
		}
	}
}

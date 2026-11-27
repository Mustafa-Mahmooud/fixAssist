package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestParseAgentNotInRepo(t *testing.T) {
	for _, out := range []string{
		`{"result":"Searched everywhere.\n\nCANNOT_FIX: NOT_IN_REPO: /LearningErrors/name, \"name may contain letters\"","is_error":false}`,
		`{"result":"**CANNOT_FIX: NOT_IN_REPO:** /LearningErrors/name, \"name may contain letters\"","is_error":false}`,
	} {
		_, err := parseAgent([]byte(out))
		e, ok := asNotInRepo(err)
		if !ok || e.searched != `/LearningErrors/name, "name may contain letters"` {
			t.Errorf("%s: %v (%q)", out, err, e.searched)
		}
	}
	// A plain CANNOT_FIX stays what it was.
	if _, err := parseAgent([]byte(`{"result":"CANNOT_FIX: needs a schema change","is_error":false}`)); err == nil {
		t.Error("CANNOT_FIX passed")
	} else if _, ok := asNotInRepo(err); ok {
		t.Error("a plain CANNOT_FIX read as not in repo")
	}
}

func TestSafeBranch(t *testing.T) {
	for _, ok := range []string{"main", "master", "dev", "feature/learning-errors", "release-1.4", "v2+fix", "user_x/y"} {
		if !safeBranch(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "-x", "--upload-pack=touch /tmp/p", "/main", ".hidden", "a..b", "a//b", "a/.b", "a@{1}", "a b", "a;b",
		"a$(x)", "main/", "main.", "x.lock", "a\nb", "a\\b", "a:b", "a~1", "a^", "a?", "a*", "a[", strings.Repeat("a", 201)} {
		if safeBranch(bad) {
			t.Errorf("%q allowed", bad)
		}
	}
}

func TestBaseFor(t *testing.T) {
	w := &worker{p: Project{Repo: t.TempDir(), Base: "master"}}
	ctx := context.Background()
	if b, err := w.baseFor(ctx, &Task{}); err != nil || b != "master" {
		t.Errorf("no branch from the server: %q %v", b, err)
	}
	if _, err := w.baseFor(ctx, &Task{Base: "--upload-pack=x"}); err == nil {
		t.Error("an option-shaped branch was used")
	}
}

func TestWhereLooked(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	ctx := context.Background()
	dir := t.TempDir()
	remote, repo := filepath.Join(dir, "remote.git"), filepath.Join(dir, "repo")
	gitT := func(d string, args ...string) {
		t.Helper()
		if out, err := git(ctx, d, args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	gitT(dir, "init", "-q", "--bare", "-b", "master", remote)
	gitT(dir, "init", "-q", "-b", "master", repo)
	gitT(repo, "config", "user.email", "t@example.com")
	gitT(repo, "config", "user.name", "t")
	commit := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		gitT(repo, "add", name)
		gitT(repo, "commit", "-q", "-m", name)
	}
	commit("a.txt")
	gitT(repo, "remote", "add", "origin", remote)
	gitT(repo, "push", "-q", "origin", "master")
	gitT(repo, "fetch", "-q", "origin")

	got := whereLooked(ctx, repo, "origin", "master")
	if !strings.HasPrefix(got, "origin/master (commit ") || strings.Contains(got, "local copy") {
		t.Errorf("clean copy: %s", got)
	}

	// On another branch, with unpushed commits and uncommitted changes.
	gitT(repo, "checkout", "-q", "-b", "learning")
	commit("b.txt")
	commit("c.txt")
	if err := os.WriteFile(filepath.Join(repo, "d.txt"), []byte("d"), 0o600); err != nil {
		t.Fatal(err)
	}
	got = whereLooked(ctx, repo, "origin", "master")
	for _, want := range []string{"is on the branch learning", "has 2 commits not on origin/master", "has changes that aren't committed", "auto-fix can't see those"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
}

func TestNotInRepoMessage(t *testing.T) {
	where := "origin/master (commit 6174f0a). Your local copy is on the branch learning, and auto-fix can't see those."
	got := notInRepoMessage(`/LearningErrors/name, "name may contain letters".`, where)
	want := `It searched for /LearningErrors/name, "name may contain letters". It looked in ` + where
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if got := notInRepoMessage("  ", "origin/main."); got != "It looked in origin/main." {
		t.Errorf("nothing searched: %s", got)
	}
}

func TestClipKeepsCharacters(t *testing.T) {
	s := strings.Repeat("خطأ", 10) // two bytes a letter
	for n := 1; n < len(s); n++ {
		if c := clip(s, n); !utf8.ValidString(c) {
			t.Fatalf("clip(%d) cut a character: %q", n, c)
		}
	}
}

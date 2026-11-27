package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// When the agent can't find the code that raises the error, the founder
// needs to know where it looked: the daemon fixes from <remote>/<base>, not
// from their working copy, so code that is only on another branch, not
// pushed or not committed is invisible to it.

// notInRepoTag is how the agent says the code isn't there (the server's
// prompt asks for "CANNOT_FIX: NOT_IN_REPO: <what it searched for>").
const notInRepoTag = "NOT_IN_REPO"

// errNotInRepo is the agent saying the code that raises the error isn't in
// the repository; searched is what it looked for.
type errNotInRepo struct{ searched string }

func (e errNotInRepo) Error() string {
	return "The agent didn't find the code that raises this error: " + e.searched
}

// baseFor is the branch to fix from: the one set on the Setup page when
// there is one, else this app's own (fixassist add --base). The server's
// name reaches git and gh as an argument, so it's checked here too.
func (w *worker) baseFor(ctx context.Context, t *Task) (string, error) {
	if t.Base == "" || t.Base == w.p.Base {
		return w.p.Base, nil
	}
	if !safeBranch(t.Base) {
		return "", fmt.Errorf("Fix Assistant asked for the branch %q, which this daemon won't use", t.Base)
	}
	if _, err := git(ctx, w.p.Repo, "check-ref-format", "--branch", t.Base); err != nil {
		return "", fmt.Errorf("%q isn't a valid branch name", t.Base)
	}
	return t.Base, nil
}

// safeBranch is the same strict form the server allows (autofix.ValidBranch):
// never an option, never a revision expression.
func safeBranch(name string) bool {
	if name == "" || len(name) > 200 || name[0] == '-' || name[0] == '/' || name[0] == '.' ||
		strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") || strings.HasSuffix(name, ".lock") ||
		strings.Contains(name, "..") || strings.Contains(name, "//") || strings.Contains(name, "/.") || strings.Contains(name, "@{") {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._/-+", c)) {
			return false
		}
	}
	return true
}

// whereLooked says what the agent searched and what in the founder's local
// copy it couldn't see: "origin/master (commit 6174f0a). Your copy is on
// dev, has 3 commits not pushed, and has uncommitted changes."
func whereLooked(ctx context.Context, repo, remote, base string) string {
	ref := remote + "/" + base
	s := ref
	if out, err := git(ctx, repo, "rev-parse", "--short", ref); err == nil {
		s += " (commit " + strings.TrimSpace(string(out)) + ")"
	}
	var notes []string
	if out, err := git(ctx, repo, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		if cur := strings.TrimSpace(string(out)); cur != "" && cur != base && cur != "HEAD" {
			notes = append(notes, "is on the branch "+cur)
		}
	}
	if out, err := git(ctx, repo, "rev-list", "--count", ref+"..HEAD"); err == nil {
		if n, _ := strconv.Atoi(strings.TrimSpace(string(out))); n > 0 {
			notes = append(notes, fmt.Sprintf("has %d commit%s not on %s", n, plural(n), ref))
		}
	}
	if out, err := git(ctx, repo, "status", "--porcelain", "--untracked-files=normal"); err == nil && strings.TrimSpace(string(out)) != "" {
		notes = append(notes, "has changes that aren't committed")
	}
	if len(notes) == 0 {
		return s + "."
	}
	return s + ". Your local copy " + joinAnd(notes) + ", and auto-fix can't see those."
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func joinAnd(xs []string) string {
	if len(xs) < 2 {
		return strings.Join(xs, "")
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}

// failNotInRepo reports a run whose code wasn't found, with where it looked.
func (w *worker) failNotInRepo(ctx context.Context, t *Task, e errNotInRepo, remote, base string) {
	msg := notInRepoMessage(e.searched, whereLooked(ctx, w.p.Repo, remote, base))
	w.logf("run %d (issue #%d) failed: code not found. %s", t.RunID, t.IssueID, msg)
	w.report(t, Event{Event: "failed", Reason: "not_in_repo", Message: msg})
}

// notInRepoMessage is what the dashboard shows: "It searched for <what>. It
// looked in <where>"; where is whereLooked's sentence.
func notInRepoMessage(searched, where string) string {
	msg := "It looked in " + where
	if s := strings.TrimRight(strings.TrimSpace(searched), ". "); s != "" {
		msg = "It searched for " + clip(s, 600) + ". " + msg
	}
	return msg
}

func asNotInRepo(err error) (errNotInRepo, bool) {
	var e errNotInRepo
	ok := errors.As(err, &e)
	return e, ok
}

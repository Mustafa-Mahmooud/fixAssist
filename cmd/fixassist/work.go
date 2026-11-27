package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	agentTimeout  = 40 * time.Minute // one Claude Code run
	heartbeatEach = 5 * time.Minute  // the server's lease is 60 minutes
	prCheckEach   = 2 * time.Minute
)

type worker struct {
	p Project
	c *client
}

func (w *worker) logf(format string, args ...any) {
	logf("[%s] "+format, append([]any{w.p.Name}, args...)...)
}

// reloadToken picks up a new token for this app from the config file
// (`fixassist add` again after the old token was revoked), so a running
// watcher doesn't keep retrying a dead token until it is restarted.
func (w *worker) reloadToken() bool {
	cfg, _, err := loadConfig()
	if err != nil {
		return false
	}
	for _, p := range cfg.Projects {
		if p.Repo == w.p.Repo && p.Server == w.p.Server && p.Token != w.p.Token {
			w.p, w.c = p, newClient(p)
			return true
		}
	}
	return false
}

// loop claims and works tasks one at a time until ctx ends.
func (w *worker) loop(ctx context.Context) {
	backoff := 5 * time.Second
	var lastPRCheck time.Time
	for ctx.Err() == nil {
		if time.Since(lastPRCheck) > prCheckEach {
			w.checkPRs(ctx)
			lastPRCheck = time.Now()
		}
		t, err := w.c.claim(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if unauthorized(err) && w.reloadToken() {
				w.logf("the agent token changed in the config: using the new one")
				backoff = 5 * time.Second
				continue
			}
			w.logf("can't reach Fix Assistant: %v (retrying in %s)", err, backoff)
			sleep(ctx, backoff)
			backoff = min(backoff*2, time.Minute)
			continue
		}
		backoff = 5 * time.Second
		if t == nil {
			continue
		}
		switch t.Kind {
		case "fix":
			w.fix(ctx, t)
		case "merge":
			w.merge(ctx, t)
		default:
			w.logf("unknown task %q: update fixassist", t.Kind)
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// report sends an event; failures are logged (the server's lease and sweep
// tidy up after a daemon that can't report).
func (w *worker) report(t *Task, e Event) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := w.c.event(ctx, t.RunID, e); err != nil {
		w.logf("run %d: couldn't report %s: %v", t.RunID, e.Event, err)
	}
}

func (w *worker) fail(t *Task, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	w.logf("run %d (issue #%d) failed: %s", t.RunID, t.IssueID, msg)
	w.report(t, Event{Event: "failed", Message: msg})
}

// fix runs Claude Code on the task in a fresh worktree off the base branch
// and opens a pull request with the change.
func (w *worker) fix(ctx context.Context, t *Task) {
	w.logf("run %d: fixing issue #%d: %s", t.RunID, t.IssueID, t.Title)
	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	go func() {
		tick := time.NewTicker(heartbeatEach)
		defer tick.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-tick.C:
				_ = w.c.event(hbCtx, t.RunID, Event{Event: "heartbeat"})
			}
		}
	}()

	repo, remote := w.p.Repo, w.p.Remote
	base, err := w.baseFor(ctx, t)
	if err != nil {
		w.fail(t, "%v", err)
		return
	}
	if _, err := git(ctx, repo, "fetch", remote, base); err != nil {
		w.fail(t, "git fetch %s %s: %v", remote, base, err)
		return
	}
	wt := filepath.Join(os.TempDir(), "fixassist", fmt.Sprintf("run-%d", t.RunID))
	_ = os.RemoveAll(wt)
	if _, err := git(ctx, repo, "worktree", "add", "-B", t.Branch, wt, remote+"/"+base); err != nil {
		w.fail(t, "git worktree add: %v", err)
		return
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = git(c, repo, "worktree", "remove", "--force", wt)
		_, _ = git(c, repo, "branch", "-D", t.Branch) // pushed already, or not wanted
	}()

	summary, err := w.agent(ctx, wt, t.Prompt)
	if e, ok := asNotInRepo(err); ok {
		w.failNotInRepo(ctx, t, e, remote, base)
		return
	}
	if err != nil {
		w.fail(t, "%v", err)
		return
	}
	if _, err := git(ctx, wt, "add", "-A"); err != nil {
		w.fail(t, "git add: %v", err)
		return
	}
	out, err := git(ctx, wt, "diff", "--cached", "--name-only")
	if err != nil {
		w.fail(t, "git diff: %v", err)
		return
	}
	files := lines(out)
	if len(files) == 0 {
		w.fail(t, "The agent didn't change anything. %s", summary)
		return
	}
	if bad := forbidden(files); len(bad) > 0 {
		w.fail(t, "The agent changed files auto-fix never touches (%s), so nothing was pushed.", strings.Join(bad, ", "))
		return
	}
	diff, err := git(ctx, wt, "diff", "--cached")
	if err != nil {
		w.fail(t, "git diff: %v", err)
		return
	}
	if why := secretIn(diff, w.p.Token); why != "" {
		w.fail(t, "The change contains %s, so nothing was pushed.", why)
		return
	}
	if w.p.Test != "" {
		if out, err := shell(ctx, wt, w.p.Test); err != nil {
			w.fail(t, "Tests failed (%s): %v\n%s", w.p.Test, err, tail(out, 1500))
			return
		}
	}
	msg := fmt.Sprintf("Fix: %s\n\n%s\n\nFix Assistant auto-fix for issue #%d (run %d).", clip(t.Title, 64), summary, t.IssueID, t.RunID)
	if _, err := git(ctx, wt, "commit", "-m", msg); err != nil {
		w.fail(t, "git commit: %v (is git user.name/user.email set?)", err)
		return
	}
	if _, err := git(ctx, wt, "push", "--force-with-lease", "-u", remote, t.Branch); err != nil {
		w.fail(t, "git push: %v", err)
		return
	}
	body := fmt.Sprintf("Fix Assistant found this error in production and Claude Code wrote this fix.\n\n**What changed:** %s\n\n**Files:** %s\n\nIssue #%d, auto-fix run %d. Fix Assistant watches for the error after the merge and reopens the issue if it comes back.",
		summary, "`"+strings.Join(files, "`, `")+"`", t.IssueID, t.RunID)
	prOut, err := run(ctx, wt, nil, "gh", "pr", "create", "--base", base, "--head", t.Branch, "--title", "Fix: "+clip(t.Title, 64), "--body", body)
	if err != nil {
		w.fail(t, "gh pr create: %v", err)
		return
	}
	prURL := lastURL(prOut)
	if prURL == "" {
		w.fail(t, "gh pr create didn't print the pull request's URL")
		return
	}
	required := requiredChecks(ctx, repo, base)
	autoMerge := false
	if t.AutoMergeAllowed && required {
		if err := enableAutoMerge(ctx, repo, prURL); err != nil {
			w.logf("run %d: auto-merge not turned on: %v", t.RunID, err)
		} else {
			autoMerge = true
		}
	}
	w.report(t, Event{Event: "pr_opened", Branch: t.Branch, PRURL: prURL, Summary: summary, Files: files, RequiredChecks: required, AutoMerge: autoMerge})
	if t.AutoMergeAllowed && required && !autoMerge {
		w.report(t, Event{Event: "note", Message: "Auto-merge couldn't be turned on. Check that “Allow auto-merge” is on in the repository's settings, then merge the pull request on GitHub."})
	}
	w.logf("run %d: pull request %s (auto-merge %v)", t.RunID, prURL, autoMerge)
}

// agent runs Claude Code headless in dir. It may read and edit files and
// look at git state; it can't push, call the network or run anything else.
// Not even the tests: it could rewrite the test script first, and the
// prompt carries text from the app's visitors. The daemon runs them after.
func (w *worker) agent(ctx context.Context, dir, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, agentTimeout)
	defer cancel()
	// Reads, Glob and Grep inside the worktree need no approval. Edit(*) and
	// Write(*) cover the worktree only; a bare "Read" would also approve
	// reads elsewhere (~/.ssh, this daemon's config). No Bash at all: even
	// git diff/log/show can read (--no-index) or write (--output) any file.
	// dontAsk denies every other call instead of prompting.
	args := []string{"-p", "--output-format", "json",
		"--permission-mode", "dontAsk",
		"--allowedTools", "Edit(*),Write(*)",
		"--disallowedTools", "Bash,WebFetch,WebSearch",
		"--max-turns", "60"}
	out, err := run(ctx, dir, strings.NewReader(prompt), "claude", args...)
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("Claude Code didn't finish within %s", agentTimeout)
	}
	if err != nil {
		return "", fmt.Errorf("Claude Code: %v", err)
	}
	return parseAgent(out)
}

// parseAgent reads `claude -p --output-format json` and the SUMMARY or
// CANNOT_FIX line the prompt asks for.
func parseAgent(out []byte) (string, error) {
	var r struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
		Subtype string `json:"subtype"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &r); err != nil {
		return "", fmt.Errorf("couldn't read Claude Code's answer: %v", err)
	}
	if r.IsError {
		return "", fmt.Errorf("Claude Code stopped (%s): %s", r.Subtype, clip(r.Result, 500))
	}
	ls := lines(r.Result)
	for i := len(ls) - 1; i >= 0; i-- {
		l := strings.Trim(ls[i], " *`_")
		if s, ok := strings.CutPrefix(l, "CANNOT_FIX:"); ok {
			s = strings.Trim(s, " *`_")
			if what, ok := strings.CutPrefix(s, notInRepoTag); ok {
				return "", errNotInRepo{searched: strings.Trim(what, " :*`_")}
			}
			return "", fmt.Errorf("The agent couldn't find a safe fix: %s", s)
		}
		if s, ok := strings.CutPrefix(l, "SUMMARY:"); ok {
			if s = strings.Trim(s, " *`_"); s != "" {
				return clip(s, 1500), nil
			}
		}
	}
	return clip(strings.TrimSpace(r.Result), 500), nil
}

// forbidden lists changed files auto-fix must never push: CI and deploy
// configuration and secrets.
func forbidden(files []string) []string {
	var bad []string
	for _, f := range files {
		l := strings.ToLower(filepath.ToSlash(f))
		name := l[strings.LastIndex(l, "/")+1:]
		switch {
		case strings.HasPrefix(l, ".github/"), strings.HasPrefix(l, ".circleci/"), strings.HasPrefix(l, ".gitlab/"),
			name == ".gitlab-ci.yml", name == ".env" || strings.HasPrefix(name, ".env."),
			name == "netlify.toml", name == "vercel.json", name == "fly.toml", name == "render.yaml":
			bad = append(bad, f)
		}
	}
	return bad
}

var reAgentToken = regexp.MustCompile(`\bfa_[A-Za-z0-9_-]{20,}`)

// secretIn looks at the added lines of a diff for things that must never be
// pushed: an agent token (this daemon's or any) or a private key. It says
// what it found, or "".
func secretIn(diff []byte, token string) string {
	for _, l := range strings.Split(string(diff), "\n") {
		if !strings.HasPrefix(l, "+") || strings.HasPrefix(l, "+++") {
			continue
		}
		switch {
		case token != "" && strings.Contains(l, token), reAgentToken.MatchString(l):
			return "a Fix Assistant agent token"
		case strings.Contains(l, "PRIVATE KEY-----"):
			return "a private key"
		}
	}
	return ""
}

// merge turns on auto-merge for a pull request a person approved.
func (w *worker) merge(ctx context.Context, t *Task) {
	base, err := w.baseFor(ctx, t)
	if err != nil {
		w.report(t, Event{Event: "note", Message: err.Error() + ". Merge the pull request on GitHub."})
		return
	}
	if !requiredChecks(ctx, w.p.Repo, base) {
		w.report(t, Event{Event: "note", Message: "The default branch doesn't require status checks, so auto-merge stays off. Merge the pull request on GitHub."})
		return
	}
	if err := enableAutoMerge(ctx, w.p.Repo, t.PRURL); err != nil {
		w.report(t, Event{Event: "note", Message: "Auto-merge couldn't be turned on (" + clip(err.Error(), 300) + "). Merge the pull request on GitHub."})
		return
	}
	w.logf("run %d: auto-merge on for %s", t.RunID, t.PRURL)
}

// checkPRs reports pull requests that were merged or closed.
func (w *worker) checkPRs(ctx context.Context) {
	prs, err := w.c.prs(ctx)
	if err != nil {
		return
	}
	for _, pr := range prs {
		out, err := run(ctx, w.p.Repo, nil, "gh", "pr", "view", pr.PRURL, "--json", "state,mergedAt")
		if err != nil {
			continue
		}
		var v struct {
			State    string    `json:"state"`
			MergedAt time.Time `json:"mergedAt"`
		}
		if json.Unmarshal(out, &v) != nil {
			continue
		}
		t := &Task{RunID: pr.RunID, IssueID: pr.IssueID}
		switch v.State {
		case "MERGED":
			w.logf("run %d: merged; Fix Assistant now checks the fix in production", pr.RunID)
			w.report(t, Event{Event: "merged", MergedAt: v.MergedAt})
		case "CLOSED":
			w.fail(t, "The pull request was closed without merging.")
		}
	}
}

func enableAutoMerge(ctx context.Context, repo, prURL string) error {
	_, err := run(ctx, repo, nil, "gh", "pr", "merge", prURL, "--auto", "--squash")
	return err
}

// requiredChecks says whether the base branch requires status checks,
// through branch protection or a ruleset. Without them `gh pr merge --auto`
// would merge at once, before any CI ran, so auto-merge stays off.
func requiredChecks(ctx context.Context, repo, base string) bool {
	b := url.PathEscape(base)
	if out, err := run(ctx, repo, nil, "gh", "api", "repos/{owner}/{repo}/branches/"+b); err == nil && protectedHasChecks(out) {
		return true
	}
	if out, err := run(ctx, repo, nil, "gh", "api", "repos/{owner}/{repo}/rules/branches/"+b); err == nil && rulesHaveChecks(out) {
		return true
	}
	return false
}

func protectedHasChecks(out []byte) bool {
	var v struct {
		Protection struct {
			RequiredStatusChecks struct {
				Contexts []string `json:"contexts"`
				Checks   []any    `json:"checks"`
			} `json:"required_status_checks"`
		} `json:"protection"`
	}
	if json.Unmarshal(out, &v) != nil {
		return false
	}
	rs := v.Protection.RequiredStatusChecks
	return len(rs.Contexts) > 0 || len(rs.Checks) > 0
}

func rulesHaveChecks(out []byte) bool {
	var rules []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(out, &rules) != nil {
		return false
	}
	for _, r := range rules {
		if r.Type == "required_status_checks" {
			return true
		}
	}
	return false
}

// --- processes -------------------------------------------------------------

func run(ctx context.Context, dir string, stdin *strings.Reader, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GIT_TERMINAL_PROMPT=0")
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return stdout.Bytes(), errors.New(tail([]byte(msg), 600))
	}
	return stdout.Bytes(), nil
}

func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return run(ctx, dir, nil, "git", args...)
}

func shell(ctx context.Context, dir, command string) ([]byte, error) {
	if runtime.GOOS == "windows" {
		return run(ctx, dir, nil, "cmd", "/C", command)
	}
	return run(ctx, dir, nil, "sh", "-c", command)
}

func defaultBranch(ctx context.Context, repo, remote string) string {
	out, err := git(ctx, repo, "symbolic-ref", "--short", "refs/remotes/"+remote+"/HEAD")
	if err == nil {
		if b, ok := strings.CutPrefix(strings.TrimSpace(string(out)), remote+"/"); ok && b != "" {
			return b
		}
	}
	return "main"
}

func lines(s any) []string {
	var str string
	switch v := s.(type) {
	case []byte:
		str = string(v)
	case string:
		str = v
	}
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(str, "\r\n", "\n"), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func lastURL(out []byte) string {
	ls := lines(out)
	for i := len(ls) - 1; i >= 0; i-- {
		if strings.HasPrefix(ls[i], "https://") {
			return ls[i]
		}
	}
	return ""
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func tail(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

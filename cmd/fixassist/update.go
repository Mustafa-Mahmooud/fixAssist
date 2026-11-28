package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/yourname/fixassist/internal/daemonver"
)

// Self-update. When every connected app's server offers a newer build (and
// allows updating), the background watcher, between fixes:
//  1. downloads the .sig and checks it against the key built into this
//     binary: it must name this platform's file and a version newer than
//     ours (never older: a signed old build can't be pushed back);
//  2. downloads the binary, checks its SHA-256 against the manifest and
//     runs it with "version" to see that it starts;
//  3. swaps the files (this one becomes .old) and starts the new one as a
//     handover child, which checks its tools and the server first and only
//     then takes over (the takeover port stops this one);
//  4. if the child doesn't take over within handoverWait, stops it, puts
//     the old file back and skips that version for a day.
// No new task is claimed while an update is under way.

// Update is a server's offer (agentapi.Update).
type Update struct {
	Version string `json:"version"`
	Auto    bool   `json:"auto"`
}

const (
	updateCheckEach = time.Minute
	handoverWait    = 90 * time.Second
	skipBadFor      = 24 * time.Hour
)

// openManifest checks a .sig with the key built into this binary. A var only
// so tests can sign with their own key; nothing at run time can change it.
var openManifest = daemonver.Open

// updater is the process's one updater; workers report offers and hold it
// off while they work.
type updater struct {
	mu      sync.Mutex
	idle    *sync.Cond
	busy    int  // polls and tasks in flight
	pausing bool // an update is waiting for busy to reach 0
	offers  map[string]*Update
	skip    map[string]time.Time
	told    string // the version a foreground watcher already mentioned

	exe      string // this binary
	logPath  string // background watcher's log; "" = in a terminal: only tell
	disabled bool   // the config says auto_update: false
}

var up = newUpdater()

func newUpdater() *updater {
	u := &updater{offers: map[string]*Update{}, skip: map[string]time.Time{}}
	u.idle = sync.NewCond(&u.mu)
	return u
}

// begin marks a poll or task in flight; false while an update is pausing
// work (the worker waits and asks again).
func (u *updater) begin() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.pausing {
		return false
	}
	u.busy++
	return true
}

func (u *updater) end() {
	u.mu.Lock()
	u.busy--
	u.mu.Unlock()
	u.idle.Broadcast()
}

// offer records what p's server offered on its last poll (nil: nothing).
func (u *updater) offer(p Project, o *Update) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.offers[p.Server+"|"+p.Repo] = o
}

// wanted is the version every app's server offers and allows, newer than
// ours and not recently failed, and a project to fetch it from; "" when
// there's nothing to do.
func (u *updater) wanted(projects []Project) (string, Project) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(projects) == 0 {
		return "", Project{}
	}
	var v string
	for _, p := range projects {
		o := u.offers[p.Server+"|"+p.Repo]
		if o == nil || !o.Auto || !daemonver.Older(version, o.Version) {
			return "", Project{}
		}
		if v == "" || daemonver.Older(o.Version, v) {
			v = o.Version // the oldest offered: every server can serve it
		}
	}
	if until, ok := u.skip[v]; ok && time.Now().Before(until) {
		return "", Project{}
	}
	return v, projects[0]
}

// run checks for an update every minute until ctx ends.
func (u *updater) run(ctx context.Context, projects []Project) {
	tick := time.NewTicker(updateCheckEach)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		v, p := u.wanted(projects)
		if v == "" {
			continue
		}
		if u.disabled {
			continue
		}
		if u.logPath == "" || !daemonver.HasKey() {
			if u.told != v {
				u.told = v
				logf("fixassist %s is available (this is %s). Run the install command from the Setup page again to update.", v, version)
			}
			continue
		}
		if err := u.update(ctx, p, v); err != nil {
			if ctx.Err() != nil {
				return // taken over: this is the old one
			}
			logf("couldn't update to %s: %v (keeping %s; trying again tomorrow)", v, err, version)
			u.mu.Lock()
			u.skip[v] = time.Now().Add(skipBadFor)
			u.mu.Unlock()
		}
	}
}

// pause stops new work and waits until none is in flight.
func (u *updater) pause(ctx context.Context) bool {
	u.mu.Lock()
	u.pausing = true
	go func() { <-ctx.Done(); u.idle.Broadcast() }()
	for u.busy > 0 && ctx.Err() == nil {
		u.idle.Wait()
	}
	u.mu.Unlock()
	return ctx.Err() == nil
}

func (u *updater) resume() {
	u.mu.Lock()
	u.pausing = false
	u.mu.Unlock()
}

// update replaces this binary with v from p's server and hands over to it.
// On success the new one takes over, ctx ends and ctx's error is returned;
// any other error means this binary is still in place and still running.
func (u *updater) update(ctx context.Context, p Project, v string) error {
	logf("updating to fixassist %s once the work in progress is done", v)
	if !u.pause(ctx) {
		return ctx.Err()
	}
	defer u.resume()
	file := daemonver.FileName(runtime.GOOS, runtime.GOARCH)
	base := strings.TrimRight(p.Server, "/") + "/download/fixassist/" + file

	sig, err := fetch(ctx, base+".sig", 64<<10)
	if err != nil {
		return fmt.Errorf("signature: %w", err)
	}
	m, err := openManifest(sig)
	if err != nil {
		return err
	}
	if m.File != file {
		return fmt.Errorf("the signature is for %s, not %s", m.File, file)
	}
	if !daemonver.Older(version, m.Version) {
		return fmt.Errorf("the signed build is %s, not newer than %s", m.Version, version)
	}
	bin, err := fetch(ctx, base, 200<<20)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if !m.Matches(bin) {
		return errors.New("the download doesn't match its signature")
	}
	newPath, oldPath := u.exe+".new", u.exe+".old"
	if runtime.GOOS == "windows" {
		newPath = strings.TrimSuffix(u.exe, ".exe") + ".new.exe"
		oldPath = strings.TrimSuffix(u.exe, ".exe") + ".old.exe"
	}
	if err := os.WriteFile(newPath, bin, 0o755); err != nil {
		return fmt.Errorf("can't write next to %s: %w", u.exe, err)
	}
	defer os.Remove(newPath)
	if err := selfTest(ctx, newPath, m.Version); err != nil {
		return err
	}
	_ = os.Remove(oldPath)
	if err := os.Rename(u.exe, oldPath); err != nil {
		return fmt.Errorf("can't move the running binary aside: %w", err)
	}
	if err := os.Rename(newPath, u.exe); err != nil {
		_ = os.Rename(oldPath, u.exe)
		return fmt.Errorf("can't put the new binary in place: %w", err)
	}

	child := exec.Command(u.exe, "watch", "--log", u.logPath, "--handover")
	detach(child)
	if err := child.Start(); err != nil {
		u.restore(oldPath)
		return fmt.Errorf("can't start the new version: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()
	logf("started fixassist %s (pid %d): it takes over once it has checked its tools and Fix Assistant", m.Version, child.Process.Pid)
	select {
	case <-ctx.Done(): // the new one asked this one to stop: done
		logf("updated to fixassist %s; the old version (%s) is stopping", m.Version, version)
		return ctx.Err()
	case err := <-exited:
		u.restore(oldPath)
		return fmt.Errorf("the new version stopped before taking over (%v)", err)
	case <-time.After(handoverWait):
		_ = child.Process.Kill()
		<-exited // Windows keeps the file locked until it has exited
		u.restore(oldPath)
		return fmt.Errorf("the new version didn't take over within %s", handoverWait)
	}
}

// restore puts the old binary back after a failed handover.
func (u *updater) restore(oldPath string) {
	bad := u.exe + ".failed"
	_ = os.Remove(bad)
	if err := os.Rename(u.exe, bad); err == nil {
		_ = os.Remove(bad)
	}
	if err := os.Rename(oldPath, u.exe); err != nil {
		logf("couldn't put the old binary back (%v): reinstall fixassist with the command on the Setup page", err)
	}
}

// selfTest runs the downloaded binary's "version".
func selfTest(ctx context.Context, path, want string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if got := strings.TrimSpace(string(out)); err != nil || got != "fixassist "+want {
		return fmt.Errorf("the new binary doesn't start (%q, %v)", got, err)
	}
	return nil
}

func fetch(ctx context.Context, url string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "fixassist/"+version)
	c := &http.Client{Timeout: 10 * time.Minute}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("too large")
	}
	return bytes.Clone(b), nil
}

// preflight is what a handover child checks before taking over: its config,
// its tools, gh, and that every app's server accepts its token.
func preflight(ctx context.Context) error {
	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	if len(cfg.Projects) == 0 {
		return errors.New("no apps connected")
	}
	for _, tool := range []string{"git", "gh", "claude"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s isn't on PATH", tool)
		}
	}
	if _, err := run(ctx, "", nil, "gh", "auth", "status"); err != nil {
		return errors.New("the GitHub CLI isn't signed in")
	}
	for _, p := range cfg.Projects {
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := newClient(p).prs(c)
		cancel()
		if err != nil {
			return fmt.Errorf("[%s] Fix Assistant: %w", p.Name, err)
		}
	}
	return nil
}

// thisBinary is the running executable's real path.
func thisBinary() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return exe
}

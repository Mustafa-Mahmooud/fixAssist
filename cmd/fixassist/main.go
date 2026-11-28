// Command fixassist is the auto-fix daemon. It runs on the founder's own
// computer, next to their code: it waits for auto-fixes the founder approved
// in Fix Assistant, runs Claude Code on each one in a separate git worktree,
// opens a pull request with the GitHub CLI and, when allowed, turns on
// auto-merge. The code never leaves the computer except as that pull request.
//
//	fixassist add --server https://fix.example.com --token fa_... --repo ~/code/shop
//	fixassist watch
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yourname/fixassist/internal/daemonver"
)

// version is the shared one, so the server knows exactly which daemon it ships.
const version = daemonver.Version

// Project is one app the daemon fixes.
type Project struct {
	Name   string `json:"name"`
	Server string `json:"server"`
	Token  string `json:"token"`
	Repo   string `json:"repo"`           // absolute path of the local clone
	Base   string `json:"base"`           // the branch pull requests target
	Remote string `json:"remote"`         // git remote to push to
	Test   string `json:"test,omitempty"` // optional command that must pass before a pull request
}

// Config is the daemon's config file.
type Config struct {
	Projects []Project `json:"projects"`
	// AutoUpdate: false stops the background watcher updating itself
	// (absent or true: it updates when every app's server allows it).
	AutoUpdate *bool `json:"auto_update,omitempty"`
}

func configPath() (string, error) {
	if p := os.Getenv("FIXASSIST_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "fixassist", "config.json"), nil
}

func loadConfig() (Config, string, error) {
	var c Config
	path, err := configPath()
	if err != nil {
		return c, "", err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, path, nil
	}
	if err != nil {
		return c, path, err
	}
	return c, path, json.Unmarshal(b, &c)
}

func saveConfig(c Config, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	// The file holds agent tokens: readable by this user only.
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func usage() {
	fmt.Fprint(os.Stderr, `fixassist: the Fix Assistant auto-fix daemon

  fixassist pair --server URL --code CODE [--repo PATH]
      Connect an app with the one-time code from the Setup page (the
      install command there runs this for you, in the current folder).
  fixassist add --server URL --token fa_... --repo PATH [--base main] [--test "npm test"]
      Connect an app: its agent token (Setup page) and its local git clone.
  fixassist list
      Show the connected apps.
  fixassist remove --repo PATH
      Disconnect an app.
  fixassist watch [--log FILE]
      Wait for approved auto-fixes and work on them. Leave it running.
  fixassist install
      Run watch in the background: from now on at every login, no window,
      restarted if it stops. Logs go to the file status shows.
  fixassist status
      Whether the background watcher is installed, and its latest log lines.
  fixassist uninstall
      Stop the background watcher and don't start it at login any more.
  fixassist version

Needs Claude Code (claude), git and the GitHub CLI (gh, signed in with gh auth login).
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "pair":
		err = cmdPair(os.Args[2:])
	case "add":
		err = cmdAdd(os.Args[2:])
	case "list":
		err = cmdList()
	case "remove":
		err = cmdRemove(os.Args[2:])
	case "watch":
		err = cmdWatch(os.Args[2:])
	case "install":
		err = cmdInstall()
	case "uninstall":
		err = cmdUninstall()
	case "status":
		err = cmdStatus()
	case "version", "--version":
		fmt.Println("fixassist", version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixassist:", err)
		if errors.Is(err, errMissingTools) {
			os.Exit(3)
		}
		os.Exit(1)
	}
}

func cmdAdd(args []string) error {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	server := fs.String("server", "", "Fix Assistant's URL")
	token := fs.String("token", os.Getenv("FIXASSIST_TOKEN"), "the app's agent token (fa_...)")
	repo := fs.String("repo", ".", "path of the app's local git clone")
	base := fs.String("base", "", "branch pull requests target (default: the remote's default branch)")
	remote := fs.String("remote", "origin", "git remote to push to")
	test := fs.String("test", "", "optional command that must pass before a pull request is opened")
	_ = fs.Parse(args)
	if *server == "" || !strings.HasPrefix(*token, "fa_") {
		return errors.New("--server and --token (fa_...) are required. Easier: on the Setup page, click \"Get my install command\" under Auto-fix and run it in your code folder")
	}
	p, err := addProject(context.Background(), *server, *token, *repo, *base, *remote, *test)
	if err != nil {
		return err
	}
	fmt.Printf("Connected %s (pull requests into %s). Now run: fixassist watch\n", p.Repo, p.Base)
	return nil
}

// gitRepo is the absolute path of repo, if it is a git repository.
func gitRepo(ctx context.Context, repo string) (string, error) {
	abs, err := filepath.Abs(repo)
	if err != nil {
		return "", err
	}
	if _, err := git(ctx, abs, "rev-parse", "--git-dir"); err != nil {
		return "", fmt.Errorf("%s is not a git repository: run this in your app's code folder (the one you cloned from GitHub)", abs)
	}
	return abs, nil
}

// addProject checks the token and saves the app to the config.
func addProject(ctx context.Context, server, token, repo, base, remote, test string) (Project, error) {
	abs, err := gitRepo(ctx, repo)
	if err != nil {
		return Project{}, err
	}
	if base == "" {
		base = defaultBranch(ctx, abs, remote)
	}
	p := Project{Server: strings.TrimRight(server, "/"), Token: token, Repo: abs, Base: base, Remote: remote, Test: test}
	c := newClient(p)
	if _, err := c.prs(ctx); err != nil {
		return p, fmt.Errorf("couldn't reach Fix Assistant with this token: %w", err)
	}
	p.Name = filepath.Base(abs)
	cfg, path, err := loadConfig()
	if err != nil {
		return p, err
	}
	kept := cfg.Projects[:0]
	for _, x := range cfg.Projects {
		if !(x.Repo == p.Repo && x.Server == p.Server) {
			kept = append(kept, x)
		}
	}
	cfg.Projects = append(kept, p)
	return p, saveConfig(cfg, path)
}

func cmdList() error {
	cfg, path, err := loadConfig()
	if err != nil {
		return err
	}
	if len(cfg.Projects) == 0 {
		fmt.Println("No apps connected. Run fixassist add.")
		return nil
	}
	fmt.Println("Config:", path)
	for _, p := range cfg.Projects {
		fmt.Printf("- %s  %s → %s  (%s, token %s…)\n", p.Name, p.Repo, p.Base, p.Server, p.Token[:min(len(p.Token), 9)])
	}
	return nil
}

func cmdRemove(args []string) error {
	fs := flag.NewFlagSet("remove", flag.ExitOnError)
	repo := fs.String("repo", ".", "path of the app's local git clone")
	_ = fs.Parse(args)
	abs, err := filepath.Abs(*repo)
	if err != nil {
		return err
	}
	cfg, path, err := loadConfig()
	if err != nil {
		return err
	}
	kept := cfg.Projects[:0]
	for _, x := range cfg.Projects {
		if x.Repo != abs {
			kept = append(kept, x)
		}
	}
	if len(kept) == len(cfg.Projects) {
		return fmt.Errorf("%s isn't connected", abs)
	}
	cfg.Projects = kept
	return saveConfig(cfg, path)
}

func cmdWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	logPath := fs.String("log", "", "append output to this file instead of the terminal (used by install)")
	handover := fs.Bool("handover", false, "started by an update: check everything works before taking over")
	_ = fs.Parse(args)
	if *logPath != "" {
		f, err := openLog(*logPath)
		if err != nil {
			return err
		}
		defer f.Close()
		out = f
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *handover {
		// The old version keeps running until this one asks it to stop, and
		// puts itself back if this one never does: check first.
		if err := preflight(ctx); err != nil {
			logf("fixassist %s can't take over: %v", version, err)
			return err
		}
	}
	ctx = takeOver(ctx)
	up.exe, up.logPath = thisBinary(), *logPath
	if *logPath != "" {
		// In the background nobody sees errors, and the service manager
		// can't always tell it stopped (Windows' conhost reports success),
		// so retry here: log what went wrong, wait a minute, try again. A
		// missing tool or a signed-out gh then fixes itself once installed.
		for {
			err := watchSafely(ctx)
			if ctx.Err() != nil {
				return nil
			}
			logf("stopped: %v (retrying in a minute)", err)
			sleep(ctx, time.Minute)
			if ctx.Err() != nil {
				return nil
			}
		}
	}
	return watch(ctx)
}

// watchSafely is watch, with a crash turned into an error.
func watchSafely(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("crashed: %v", r)
		}
	}()
	if err = watch(ctx); err == nil {
		err = errors.New("watch ended")
	}
	return err
}

func watch(ctx context.Context) error {
	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	if len(cfg.Projects) == 0 {
		return errors.New("no apps connected: run fixassist add first")
	}
	for _, tool := range []string{"git", "gh", "claude"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s isn't installed or isn't on PATH", tool)
		}
	}
	if _, err := run(ctx, "", nil, "gh", "auth", "status"); err != nil {
		return errors.New("the GitHub CLI isn't signed in: run gh auth login")
	}
	logf("fixassist %s (pid %d) watching %d app(s). Press Ctrl+C to stop.", version, os.Getpid(), len(cfg.Projects))
	up.disabled = cfg.AutoUpdate != nil && !*cfg.AutoUpdate
	updCtx, stopUpdates := context.WithCancel(ctx)
	defer stopUpdates()
	go up.run(updCtx, cfg.Projects)
	var wg sync.WaitGroup
	for _, p := range cfg.Projects {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A crash on one app's task must not stop the others: log it
			// and start that app's loop again.
			for ctx.Err() == nil {
				func() {
					defer func() {
						if r := recover(); r != nil {
							logf("[%s] crashed: %v (restarting)", p.Name, r)
							sleep(ctx, 10*time.Second)
						}
					}()
					(&worker{p: p, c: newClient(p)}).loop(ctx)
				}()
			}
		}()
	}
	wg.Wait()
	return nil
}

// out is where watch writes: the terminal, or the log file.
var out io.Writer = os.Stdout

var outMu sync.Mutex

func logf(format string, args ...any) {
	outMu.Lock()
	defer outMu.Unlock()
	fmt.Fprintf(out, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

// maxLog is when the log starts over (the previous one is kept as .old).
const maxLog = 5 << 20

func openLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if st, err := os.Stat(path); err == nil && st.Size() > maxLog {
		_ = os.Rename(path, path+".old")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}

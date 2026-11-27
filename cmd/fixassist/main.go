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
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const version = "0.1.0"

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

  fixassist add --server URL --token fa_... --repo PATH [--base main] [--test "npm test"]
      Connect an app: its agent token (Setup page) and its local git clone.
  fixassist list
      Show the connected apps.
  fixassist remove --repo PATH
      Disconnect an app.
  fixassist watch
      Wait for approved auto-fixes and work on them. Leave it running.
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
	case "add":
		err = cmdAdd(os.Args[2:])
	case "list":
		err = cmdList()
	case "remove":
		err = cmdRemove(os.Args[2:])
	case "watch":
		err = cmdWatch(os.Args[2:])
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
		return errors.New("--server and --token (fa_...) are required")
	}
	abs, err := filepath.Abs(*repo)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if _, err := git(ctx, abs, "rev-parse", "--git-dir"); err != nil {
		return fmt.Errorf("%s is not a git repository", abs)
	}
	if *base == "" {
		*base = defaultBranch(ctx, abs, *remote)
	}
	p := Project{Server: strings.TrimRight(*server, "/"), Token: *token, Repo: abs, Base: *base, Remote: *remote, Test: *test}
	c := newClient(p)
	if _, err := c.prs(ctx); err != nil {
		return fmt.Errorf("couldn't reach Fix Assistant with this token: %w", err)
	}
	p.Name = filepath.Base(abs)
	cfg, path, err := loadConfig()
	if err != nil {
		return err
	}
	kept := cfg.Projects[:0]
	for _, x := range cfg.Projects {
		if !(x.Repo == p.Repo && x.Server == p.Server) {
			kept = append(kept, x)
		}
	}
	cfg.Projects = append(kept, p)
	if err := saveConfig(cfg, path); err != nil {
		return err
	}
	fmt.Printf("Connected %s (pull requests into %s). Now run: fixassist watch\n", abs, p.Base)
	return nil
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
	_ = fs.Parse(args)
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if _, err := run(ctx, "", nil, "gh", "auth", "status"); err != nil {
		return errors.New("the GitHub CLI isn't signed in: run gh auth login")
	}
	logf("fixassist %s watching %d app(s). Press Ctrl+C to stop.", version, len(cfg.Projects))
	var wg sync.WaitGroup
	for _, p := range cfg.Projects {
		wg.Add(1)
		go func() {
			defer wg.Done()
			(&worker{p: p, c: newClient(p)}).loop(ctx)
		}()
	}
	wg.Wait()
	return nil
}

func logf(format string, args ...any) {
	fmt.Printf("%s %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

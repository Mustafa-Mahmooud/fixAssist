package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// fixassist pair: what the one-line install from the Setup page runs. It
// checks the tools fixassist needs, trades the one-time code for an agent
// token and connects the current folder, so nobody copies a token or a path.
//
//	fixassist pair --server https://fix.example.com --code K7QM4-X2PRA [--repo .]

// errMissingTools: paired, but fixassist can't run fixes yet. The install
// script skips "fixassist install" on exit code 3.
var errMissingTools = errors.New("connected, but some tools are missing (see above)")

// toolProblem is one missing prerequisite and how to fix it.
type toolProblem struct{ What, Fix string }

// lookPath and runTool are replaced in tests.
var (
	lookPath = exec.LookPath
	runTool  = func(ctx context.Context, name string, args ...string) error {
		_, err := run(ctx, "", nil, name, args...)
		return err
	}
)

// checkTools lists what's missing to run auto-fixes on this computer.
func checkTools(ctx context.Context) []toolProblem {
	var out []toolProblem
	if _, err := lookPath("git"); err != nil {
		out = append(out, toolProblem{"git isn't installed", "Install it from https://git-scm.com/downloads"})
	}
	if _, err := lookPath("gh"); err != nil {
		out = append(out, toolProblem{"The GitHub CLI (gh) isn't installed", "Install it from https://cli.github.com, then run: gh auth login"})
	} else if runTool(ctx, "gh", "auth", "status") != nil {
		out = append(out, toolProblem{"The GitHub CLI isn't signed in", "Run: gh auth login"})
	}
	if _, err := lookPath("claude"); err != nil {
		out = append(out, toolProblem{"Claude Code (claude) isn't installed", "Install it from https://docs.anthropic.com/en/docs/claude-code, then run claude once to sign in"})
	}
	return out
}

func cmdPair(args []string) error {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	server := fs.String("server", "", "Fix Assistant's URL")
	code := fs.String("code", "", "the one-time code from the Setup page")
	repo := fs.String("repo", ".", "path of the app's local git clone")
	base := fs.String("base", "", "branch pull requests target (default: the remote's default branch)")
	_ = fs.Parse(args)
	if *server == "" || *code == "" {
		return errors.New("--server and --code are required: copy the command from the Setup page")
	}
	ctx := context.Background()

	// The folder first: a wrong folder shouldn't use up the code.
	abs, err := gitRepo(ctx, *repo)
	if err != nil {
		return err
	}
	problems := checkTools(ctx)

	host, _ := os.Hostname()
	res, err := redeem(ctx, strings.TrimRight(*server, "/"), *code, host)
	if err != nil {
		return err
	}
	p, err := addProject(ctx, *server, res.Token, abs, *base, "origin", "")
	if err != nil {
		return err
	}
	fmt.Printf("Connected %s to %s in Fix Assistant (pull requests into %s).\n", p.Repo, res.Project, p.Base)
	if len(problems) > 0 {
		fmt.Println()
		fmt.Println("Before fixassist can fix issues on this computer:")
		for _, pr := range problems {
			fmt.Printf("  - %s. %s\n", pr.What, pr.Fix)
		}
		fmt.Println("\nThen run: fixassist install")
		return errMissingTools
	}
	return nil
}

type pairResult struct {
	Token   string `json:"token"`
	Project string `json:"project"`
}

func redeem(ctx context.Context, server, code, host string) (pairResult, error) {
	body, _ := json.Marshal(map[string]string{"code": code, "hostname": host})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server+"/agent/v1/pair", bytes.NewReader(body))
	if err != nil {
		return pairResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "fixassist/"+version)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return pairResult{}, fmt.Errorf("couldn't reach Fix Assistant at %s: %w", server, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return pairResult{}, errors.New(e.Error)
		}
		return pairResult{}, fmt.Errorf("pairing failed: %s", resp.Status)
	}
	var out pairResult
	if err := json.Unmarshal(b, &out); err != nil || !strings.HasPrefix(out.Token, "fa_") {
		return pairResult{}, errors.New("pairing failed: unexpected answer from the server")
	}
	return out, nil
}

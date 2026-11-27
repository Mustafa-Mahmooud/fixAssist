package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

// Task is an auto-fix the server hands the daemon (agentapi.Task).
type Task struct {
	Kind             string `json:"kind"` // fix, merge
	RunID            int64  `json:"run_id"`
	IssueID          int64  `json:"issue_id"`
	Title            string `json:"title"`
	Prompt           string `json:"prompt"`
	Branch           string `json:"branch"`
	PRURL            string `json:"pr_url"`
	AutoMergeAllowed bool   `json:"auto_merge_allowed"`
	Base             string `json:"base"` // branch to fix from, set on the Setup page ("": this app's own)
}

// PRView is an open auto-fix pull request (agentapi.PRView).
type PRView struct {
	RunID   int64  `json:"run_id"`
	IssueID int64  `json:"issue_id"`
	State   string `json:"state"`
	PRURL   string `json:"pr_url"`
}

// Event reports progress on a run (agentapi.RunEvent).
type Event struct {
	Event          string    `json:"event"`
	Branch         string    `json:"branch,omitempty"`
	PRURL          string    `json:"pr_url,omitempty"`
	Summary        string    `json:"summary,omitempty"`
	Files          []string  `json:"files,omitempty"`
	RequiredChecks bool      `json:"required_checks"`
	AutoMerge      bool      `json:"auto_merge"`
	Message        string    `json:"message,omitempty"`
	Reason         string    `json:"reason,omitempty"` // failed: not_in_repo
	MergedAt       time.Time `json:"merged_at,omitzero"`
}

type client struct {
	base, token, host string
	http              *http.Client
}

const claimWait = 25 // seconds, the server's MaxClaimWait

func newClient(p Project) *client {
	host, _ := os.Hostname()
	return &client{base: p.Server, token: p.Token, host: host, http: &http.Client{Timeout: (claimWait + 20) * time.Second}}
}

func (c *client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", "fixassist/"+version)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		msg := resp.Status
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			msg += ": " + e.Error
		}
		return &statusError{code: resp.StatusCode, msg: msg}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

// statusError is a non-200 answer from Fix Assistant.
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

// unauthorized says the server rejected the token (revoked or replaced).
func unauthorized(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.code == http.StatusUnauthorized
}

// claim long-polls for the next task; nil when there is none.
func (c *client) claim(ctx context.Context) (*Task, error) {
	var out struct {
		Task *Task `json:"task"`
	}
	err := c.do(ctx, http.MethodPost, "/agent/v1/autofix/claim", map[string]any{"hostname": c.host, "version": version, "wait": claimWait}, &out)
	return out.Task, err
}

func (c *client) prs(ctx context.Context) ([]PRView, error) {
	var out struct {
		PRs []PRView `json:"prs"`
	}
	q := url.Values{"hostname": {c.host}, "version": {version}}
	err := c.do(ctx, http.MethodGet, "/agent/v1/autofix/prs?"+q.Encode(), nil, &out)
	return out.PRs, err
}

func (c *client) event(ctx context.Context, runID int64, e Event) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/agent/v1/autofix/runs/%d", runID), e, nil)
}

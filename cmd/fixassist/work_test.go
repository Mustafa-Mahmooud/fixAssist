package main

import (
	"strings"
	"testing"
)

func TestParseAgent(t *testing.T) {
	for _, c := range []struct {
		out, want, err string
	}{
		{`{"type":"result","subtype":"success","is_error":false,"result":"Looked at it.\n\nSUMMARY: Widened the price column to decimal(18,2)."}`, "Widened the price column to decimal(18,2).", ""},
		{`{"result":"**SUMMARY:** Guarded the null profile.","is_error":false}`, "Guarded the null profile.", ""},
		{`{"result":"CANNOT_FIX: the bug is in a third-party service","is_error":false}`, "", "couldn't find a safe fix: the bug is in a third-party service"},
		{`{"result":"max turns","is_error":true,"subtype":"error_max_turns"}`, "", "error_max_turns"},
		{`not json`, "", "couldn't read"},
		{`{"result":"Changed the query.","is_error":false}`, "Changed the query.", ""},
	} {
		got, err := parseAgent([]byte(c.out))
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: err = %v, want %q", c.out, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v", c.out, got, err)
		}
	}
}

func TestForbidden(t *testing.T) {
	got := forbidden([]string{"src/app.ts", ".github/workflows/ci.yml", ".env", "config/.env.production", "web/.envrc.md", "vercel.json", "docs/github.md"})
	if strings.Join(got, ",") != ".github/workflows/ci.yml,.env,config/.env.production,vercel.json" {
		t.Errorf("forbidden = %v", got)
	}
}

func TestRequiredChecks(t *testing.T) {
	if !protectedHasChecks([]byte(`{"name":"main","protected":true,"protection":{"enabled":true,"required_status_checks":{"enforcement_level":"non_admins","contexts":["ci"],"checks":[{"context":"ci"}]}}}`)) {
		t.Error("branch protection with checks")
	}
	if protectedHasChecks([]byte(`{"name":"main","protected":false,"protection":{"enabled":false,"required_status_checks":{"enforcement_level":"off","contexts":[],"checks":[]}}}`)) {
		t.Error("unprotected branch")
	}
	if !rulesHaveChecks([]byte(`[{"type":"pull_request"},{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"test"}]}}]`)) {
		t.Error("ruleset with checks")
	}
	if rulesHaveChecks([]byte(`[{"type":"deletion"}]`)) || rulesHaveChecks([]byte(`{"message":"Not Found"}`)) {
		t.Error("no required checks")
	}
}

func TestLastURL(t *testing.T) {
	out := "Creating pull request for fixassist/issue-1-run-2 into main in acme/shop\n\nhttps://github.com/acme/shop/pull/12\n"
	if got := lastURL([]byte(out)); got != "https://github.com/acme/shop/pull/12" {
		t.Errorf("lastURL = %q", got)
	}
}

func TestSecretIn(t *testing.T) {
	tok := "fa_abcdefghijklmnopqrstuvwxyz012345"
	for _, c := range []struct{ diff, want string }{
		{"+++ b/src/a.ts\n+const x = 1\n-const y = '" + tok + "'\n", ""}, // removed lines don't count
		{"+// " + tok + "\n", "a Fix Assistant agent token"},
		{"+const k = 'fa_ZZZZZZZZZZZZZZZZZZZZZZZZZZZZ'\n", "a Fix Assistant agent token"},
		{"+-----BEGIN OPENSSH PRIVATE KEY-----\n", "a private key"},
	} {
		if got := secretIn([]byte(c.diff), tok); got != c.want {
			t.Errorf("%q: got %q, want %q", c.diff, got, c.want)
		}
	}
}

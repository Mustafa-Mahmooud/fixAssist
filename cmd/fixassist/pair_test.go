package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckTools(t *testing.T) {
	defer func(l func(string) (string, error), r func(context.Context, string, ...string) error) {
		lookPath, runTool = l, r
	}(lookPath, runTool)
	missing := map[string]bool{"claude": true}
	lookPath = func(name string) (string, error) {
		if missing[name] {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + name, nil
	}
	runTool = func(context.Context, string, ...string) error { return errors.New("not logged in") }
	got := checkTools(context.Background())
	if len(got) != 2 || !strings.Contains(got[0].What, "isn't signed in") || !strings.Contains(got[0].Fix, "gh auth login") || !strings.Contains(got[1].What, "Claude Code") {
		t.Errorf("problems = %+v", got)
	}
	missing = map[string]bool{}
	runTool = func(context.Context, string, ...string) error { return nil }
	if got := checkTools(context.Background()); len(got) != 0 {
		t.Errorf("all there: %+v", got)
	}
}

func TestRedeem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if r.URL.Path != "/agent/v1/pair" || in["code"] != "K7QM4-X2PRA" {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"this code is wrong, expired or already used: get a new one on the Setup page"}`))
			return
		}
		_, _ = w.Write([]byte(`{"token":"fa_abc","project":"Shop","project_id":1}`))
	}))
	defer srv.Close()
	res, err := redeem(context.Background(), srv.URL, "K7QM4-X2PRA", "laptop")
	if err != nil || res.Token != "fa_abc" || res.Project != "Shop" {
		t.Fatalf("redeem: %+v %v", res, err)
	}
	if _, err := redeem(context.Background(), srv.URL, "WRONG", "laptop"); err == nil || !strings.Contains(err.Error(), "get a new one on the Setup page") {
		t.Errorf("bad code error: %v", err)
	}
}

package web_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yourname/fixassist/internal/web"
)

func TestDownloads(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fixassist-windows-amd64.exe"), []byte("MZ-binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("no"), 0o644); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&web.Downloads{Dir: dir, BaseURL: "https://fix.example.com/"}).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	get := func(path string) (*http.Response, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}

	resp, b := get("/download/fixassist/fixassist-windows-amd64.exe")
	if resp.StatusCode != 200 || b != "MZ-binary" || !strings.Contains(resp.Header.Get("Content-Disposition"), `filename="fixassist.exe"`) {
		t.Errorf("windows download: %d %q %q", resp.StatusCode, b, resp.Header.Get("Content-Disposition"))
	}
	// Known but not built, unknown names and path tricks: 404.
	for _, p := range []string{"/download/fixassist/fixassist-linux-amd64", "/download/fixassist/secret.txt", "/download/fixassist/..%2Fsecret.txt"} {
		if resp, _ := get(p); resp.StatusCode != 404 {
			t.Errorf("%s: %d, want 404", p, resp.StatusCode)
		}
	}

	_, ps1 := get("/install/fixassist.ps1")
	_, sh := get("/install/fixassist.sh")
	for name, s := range map[string]string{"ps1": ps1, "sh": sh} {
		if !strings.Contains(s, "https://fix.example.com/download/fixassist/fixassist-") || strings.Contains(s, "{{BASE}}") {
			t.Errorf("%s script doesn't point at the server:\n%s", name, s)
		}
	}
	if !strings.Contains(ps1, "SetEnvironmentVariable('Path'") || !strings.HasPrefix(sh, "#!/bin/sh") {
		t.Error("scripts look wrong")
	}

	// Every build the Setup page offers has a matching Dockerfile target.
	docker, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range web.DownloadFiles {
		target := strings.TrimSuffix(strings.TrimPrefix(f.File, "fixassist-"), ".exe")
		if !strings.Contains(string(docker), strings.Replace(target, "-", "/", 1)) {
			t.Errorf("Dockerfile doesn't build %s", f.File)
		}
	}
}

// Signatures are served next to the binaries, and only a build where every
// download has one is offered to daemons as an update.
func TestDownloadsSigned(t *testing.T) {
	dir := t.TempDir()
	d := &web.Downloads{Dir: dir, BaseURL: "https://fix.example.com"}
	for _, f := range web.DownloadFiles {
		if err := os.WriteFile(filepath.Join(dir, f.File), []byte("bin"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if d.Signed() {
		t.Fatal("unsigned downloads offered as an update")
	}
	for i, f := range web.DownloadFiles {
		if i == len(web.DownloadFiles)-1 {
			if d.Signed() {
				t.Fatal("offered with a signature missing")
			}
		}
		if err := os.WriteFile(filepath.Join(dir, f.File+".sig"), []byte("manifest\nsig\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if !d.Signed() || (&web.Downloads{}).Signed() {
		t.Fatal("Signed is wrong")
	}
	mux := http.NewServeMux()
	d.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/download/fixassist/fixassist-linux-amd64.sig")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "manifest\nsig\n" || resp.Header.Get("Content-Disposition") != "" {
		t.Errorf(".sig: %d %q %q", resp.StatusCode, b, resp.Header.Get("Content-Disposition"))
	}
	if resp, _ := http.Get(srv.URL + "/download/fixassist/other.sig"); resp.StatusCode != 404 {
		t.Errorf("unknown .sig: %d", resp.StatusCode)
	}
}

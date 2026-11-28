package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yourname/fixassist/internal/daemonver"
)

func TestUpdateWanted(t *testing.T) {
	a := Project{Server: "https://fix.example.com", Repo: "/code/a"}
	b := Project{Server: "https://fix.example.com", Repo: "/code/b"}
	newer := "99.0.0"
	u := newUpdater()
	if v, _ := u.wanted([]Project{a, b}); v != "" {
		t.Fatal("an update before any poll")
	}
	u.offer(a, &Update{Version: newer, Auto: true})
	if v, _ := u.wanted([]Project{a, b}); v != "" {
		t.Fatal("updated before every app's server was heard from")
	}
	u.offer(b, &Update{Version: newer, Auto: false})
	if v, _ := u.wanted([]Project{a, b}); v != "" {
		t.Fatal("updated although one app turned it off")
	}
	u.offer(b, &Update{Version: newer, Auto: true})
	if v, p := u.wanted([]Project{a, b}); v != newer || p != a {
		t.Fatalf("wanted = %q %v", v, p)
	}
	u.offer(b, &Update{Version: version, Auto: true}) // that server has nothing newer
	if v, _ := u.wanted([]Project{a, b}); v != "" {
		t.Fatal("updated past what one server can serve")
	}
	u.offer(b, &Update{Version: newer, Auto: true})
	u.skip[newer] = time.Now().Add(time.Hour)
	if v, _ := u.wanted([]Project{a, b}); v != "" {
		t.Fatal("retried a version that just failed")
	}
}

func TestUpdatePausesWork(t *testing.T) {
	u := newUpdater()
	if !u.begin() {
		t.Fatal("begin refused")
	}
	paused := make(chan bool)
	go func() { paused <- u.pause(context.Background()) }()
	select {
	case <-paused:
		t.Fatal("paused while a task was in flight")
	case <-time.After(100 * time.Millisecond):
	}
	if u.begin() {
		t.Fatal("new work started while an update waits")
	}
	u.end()
	if !<-paused {
		t.Fatal("pause failed")
	}
	u.resume()
	if !u.begin() {
		t.Fatal("work didn't resume")
	}
}

// updateFixture serves one platform's binary and .sig, signed with a test key.
type updateFixture struct {
	t        *testing.T
	priv     ed25519.PrivateKey
	sig, bin []byte
	srv      *httptest.Server
	u        *updater
	exe      string
}

func newUpdateFixture(t *testing.T) *updateFixture {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	saved := openManifest
	openManifest = func(sig []byte) (daemonver.Manifest, error) {
		return daemonver.OpenWith(sig, base64.StdEncoding.EncodeToString(pub))
	}
	t.Cleanup(func() { openManifest = saved })
	out = io.Discard
	f := &updateFixture{t: t, priv: priv}
	file := daemonver.FileName(runtime.GOOS, runtime.GOARCH)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download/fixassist/" + file + ".sig":
			_, _ = w.Write(f.sig)
		case "/download/fixassist/" + file:
			_, _ = w.Write(f.bin)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	dir := t.TempDir()
	f.exe = filepath.Join(dir, "fixassist.exe")
	if err := os.WriteFile(f.exe, []byte("the old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.u = newUpdater()
	f.u.exe, f.u.logPath = f.exe, filepath.Join(dir, "fixassist.log")
	return f
}

func (f *updateFixture) serve(file, ver string, bin []byte) {
	f.bin = bin
	f.sig = daemonver.Sign(f.priv, file, ver, bin)
}

// update runs one update and checks the old binary is still in place, alone.
func (f *updateFixture) refused(want string) {
	f.t.Helper()
	err := f.u.update(context.Background(), Project{Server: f.srv.URL}, "99.0.0")
	if err == nil || !strings.Contains(err.Error(), want) {
		f.t.Fatalf("update: %v, want %q", err, want)
	}
	if b, _ := os.ReadFile(f.exe); string(b) != "the old binary" {
		f.t.Fatalf("the binary changed: %q", b)
	}
	if ls, _ := os.ReadDir(filepath.Dir(f.exe)); len(ls) != 1 {
		names := []string{}
		for _, e := range ls {
			names = append(names, e.Name())
		}
		f.t.Fatalf("left files behind: %v", names)
	}
	if !f.u.begin() {
		f.t.Fatal("work stayed paused after a refused update")
	}
	f.u.end()
}

func TestUpdateRefuses(t *testing.T) {
	f := newUpdateFixture(t)
	file := daemonver.FileName(runtime.GOOS, runtime.GOARCH)

	f.serve(file, "0.0.1", []byte("old build"))
	f.refused("not newer") // a validly signed older build: never a downgrade

	f.serve("fixassist-plan9-amd64", "99.0.0", []byte("x"))
	f.refused("not " + file)

	f.serve(file, "99.0.0", []byte("the signed one"))
	f.bin = []byte("something else")
	f.refused("doesn't match")

	f.serve(file, "99.0.0", []byte("x"))
	f.sig = daemonver.Sign(f.priv, file, "99.0.0", []byte("x"))[:20] // cut off
	f.refused("signature")

	f.serve(file, "99.0.0", []byte("not a program"))
	f.refused("doesn't start")
}

// A build that starts but never takes over: the old binary goes back.
func TestUpdateRollsBack(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	f := newUpdateFixture(t)
	src := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(src, []byte(`package main

import ("fmt"; "os")

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" { fmt.Println("fixassist 99.0.0"); return }
	os.Exit(3) // "watch --handover": can't take over
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	built := filepath.Join(t.TempDir(), "fake"+map[bool]string{true: ".exe"}[runtime.GOOS == "windows"])
	if out, err := exec.Command("go", "build", "-o", built, src).CombinedOutput(); err != nil {
		t.Fatalf("go build: %v %s", err, out)
	}
	bin, _ := os.ReadFile(built)
	f.serve(daemonver.FileName(runtime.GOOS, runtime.GOARCH), "99.0.0", bin)
	f.refused("stopped before taking over")
}

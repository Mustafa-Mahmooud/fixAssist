package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// One watcher per computer. Two watchers on one config poll with the same
// tokens and race for the same runs, and an old one keeps an old binary and
// an old token alive (a reinstall or `fixassist add` doesn't reach it). So
// the newest watcher takes over: it holds a localhost port, and a watcher
// that starts later asks the one holding it to stop, then takes the port.

// controlAddr is the takeover port. Only this computer can reach it, and
// all it accepts is "stop".
// FIXASSIST_CONTROL_ADDR moves it, to run a second, separate watcher (with
// its own FIXASSIST_CONFIG) on the same computer, e.g. to try a build.
var controlAddr = func() string {
	if a := os.Getenv("FIXASSIST_CONTROL_ADDR"); strings.HasPrefix(a, "127.0.0.1:") {
		return a
	}
	return "127.0.0.1:47815"
}()

const takeoverHello = "fixassist"

// takeOver makes this the only watcher. The returned context ends when a
// newer watcher takes over. When the port is held by something that isn't
// fixassist, it warns and carries on unguarded rather than not running.
func takeOver(ctx context.Context) context.Context {
	ln, err := net.Listen("tcp", controlAddr)
	if err != nil {
		old, stopped := askToStop()
		if !stopped {
			logf("warning: %s is in use by another program, so a second fixassist watch wouldn't be noticed", controlAddr)
			return ctx
		}
		logf("took over from the fixassist watcher already running (pid %s)", old)
		for deadline := time.Now().Add(30 * time.Second); ; {
			if ln, err = net.Listen("tcp", controlAddr); err == nil || time.Now().After(deadline) {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		if err != nil {
			logf("warning: the previous watcher didn't release %s: %v", controlAddr, err)
			return ctx
		}
	}
	ctx, cancel := context.WithCancelCause(ctx)
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			line, _ := bufio.NewReader(conn).ReadString('\n')
			if strings.TrimSpace(line) == "ping" {
				fmt.Fprintf(conn, "%s %d %s\n", takeoverHello, os.Getpid(), version)
				conn.Close()
				continue
			}
			if strings.TrimSpace(line) != "stop" {
				conn.Close()
				continue
			}
			fmt.Fprintf(conn, "%s %d\n", takeoverHello, os.Getpid())
			conn.Close()
			logf("a newer fixassist watch started: stopping this one (pid %d)", os.Getpid())
			cancel(errTakenOver)
			return
		}
	}()
	return ctx
}

var errTakenOver = errors.New("a newer watcher took over")

// askToStop asks the watcher on controlAddr to stop; it returns that
// watcher's pid and whether it was one.
func askToStop() (string, bool) {
	conn, err := net.DialTimeout("tcp", controlAddr, 2*time.Second)
	if err != nil {
		return "", false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintln(conn, "stop"); err != nil {
		return "", false
	}
	reply, _ := bufio.NewReader(conn).ReadString('\n')
	rest, ok := strings.CutPrefix(strings.TrimSpace(reply), takeoverHello+" ")
	pid, _, _ := strings.Cut(rest, " ")
	return pid, ok
}

// runningWatcher asks the watcher on controlAddr for its pid and version
// ("" when none answers; version "" from one before 0.4.0, which doesn't
// know "ping" and just hangs up).
func runningWatcher() (pid, ver string) {
	conn, err := net.DialTimeout("tcp", controlAddr, 2*time.Second)
	if err != nil {
		return "", ""
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := fmt.Fprintln(conn, "ping"); err != nil {
		return "", ""
	}
	reply, _ := bufio.NewReader(conn).ReadString('\n')
	f := strings.Fields(reply)
	if len(f) < 3 || f[0] != takeoverHello {
		return "?", "" // something is listening: an older watcher
	}
	return f[1], f[2]
}

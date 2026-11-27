package main

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestTakeOver(t *testing.T) {
	// A free port of our own, so the test doesn't meet a real watcher.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	controlAddr = ln.Addr().String()
	ln.Close()
	out = io.Discard

	older := takeOver(context.Background())
	if older.Err() != nil {
		t.Fatal("the first watcher stopped by itself")
	}
	newer := takeOver(context.Background())
	select {
	case <-older.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the older watcher kept running after a newer one started")
	}
	if context.Cause(older) != errTakenOver {
		t.Errorf("older stopped with %v", context.Cause(older))
	}
	if newer.Err() != nil {
		t.Fatal("the newer watcher stopped")
	}

	// Something that isn't fixassist on the port: run unguarded, don't hang.
	ln, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	controlAddr = ln.Addr().String()
	if ctx := takeOver(context.Background()); ctx.Err() != nil {
		t.Error("a foreign program on the port stopped the watcher")
	}
}

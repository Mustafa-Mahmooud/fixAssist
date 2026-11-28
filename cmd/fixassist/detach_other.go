//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detach starts the handover child in its own session. Under launchd or
// systemd the service restarts once the old process exits; the restarted
// one (the new binary, now in place) takes over from this child.
func detach(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

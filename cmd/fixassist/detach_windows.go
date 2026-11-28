package main

import (
	"os/exec"
	"syscall"
)

// detach starts the handover child on its own, without a window. It gets a
// hidden console (CREATE_NO_WINDOW), not none (DETACHED_PROCESS): the git,
// gh and claude it runs inherit that console, so no window flashes open.
func detach(c *exec.Cmd) {
	const createNoWindow, createNewProcessGroup = 0x08000000, 0x00000200
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow | createNewProcessGroup, HideWindow: true}
}

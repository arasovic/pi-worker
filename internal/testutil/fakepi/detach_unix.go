//go:build darwin || linux

package main

import (
	"os/exec"
	"syscall"
)

// detachProcess configures cmd to start in its own session (and therefore
// its own process group), outside the parent's inherited boundary. Only the
// Unix lifecycle tests use the detached spawn.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

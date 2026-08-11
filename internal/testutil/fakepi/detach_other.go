//go:build !darwin && !linux

package main

import "os/exec"

// detachProcess is a no-op on platforms without the Unix session
// primitive; no test on those platforms requests a detached spawn.
func detachProcess(cmd *exec.Cmd) {}

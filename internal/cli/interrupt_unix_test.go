//go:build darwin || linux

package cli

import (
	"bytes"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestInterruptHelper is the subprocess entry point for
// TestFirstInterruptWhileReadingStdinPromptExitsPromptly: re-executing the
// test binary with this filter and the helper environment runs the real
// cli.Main against a real open stdin pipe, so the interrupt test exercises
// actual signal semantics instead of an in-process context.
func TestInterruptHelper(t *testing.T) {
	if os.Getenv(interruptHelperEnv) != "1" {
		return
	}
	// --model only, no --task: the prompt must be read from stdin, which is
	// an open pipe that never delivers data, so the process blocks on the
	// read until the parent signals it.
	os.Exit(Main([]string{"run", "--model", "acme/m-1"}, os.Stdin, os.Stdout, os.Stderr))
}

// TestFirstInterruptWhileReadingStdinPromptExitsPromptly is the regression
// test for the first Ctrl-C while pi-worker is blocked reading a stdin
// prompt: with no child process and no cleanup-requiring work yet, the
// interrupt must terminate the process promptly with default SIGINT
// behavior. Before the fix, signal interception was installed before the
// blocking read, so the first Ctrl-C was swallowed and the process hung
// forever on the read. The child must die by SIGINT, never reach the
// worker, and never produce a cancelled result.
func TestFirstInterruptWhileReadingStdinPromptExitsPromptly(t *testing.T) {
	if os.Getenv(interruptHelperEnv) == "1" {
		return // the helper test owns this environment
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestInterruptHelper$")
	// The helper needs only its test marker. Keep the environment minimal so
	// this signal test never materializes or forwards the developer's host
	// credentials.
	cmd.Env = []string{interruptHelperEnv + "=1"}
	var childOutput bytes.Buffer
	cmd.Stdout = &childOutput
	cmd.Stderr = &childOutput
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("create stdin pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	defer stdin.Close()

	// The helper is either still starting or blocked on the prompt read;
	// both states must die from the default SIGINT behavior. The exit bound
	// below is what proves the interrupt is not swallowed: the regression
	// hangs forever, so any prompt exit fails the old code.
	time.Sleep(300 * time.Millisecond)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("signal helper: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("helper exited successfully; want death by SIGINT")
		}
		status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
		if !ok {
			t.Fatalf("no wait status: %v", cmd.ProcessState)
		}
		if !status.Signaled() || status.Signal() != syscall.SIGINT {
			t.Fatalf("helper exit = %v, want death by SIGINT (default interrupt behavior); output: %s", cmd.ProcessState, childOutput.String())
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("pi-worker hung after the first interrupt while reading the stdin prompt")
	}
}

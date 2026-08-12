//go:build darwin || linux

package cli

import (
	"syscall"
	"testing"
	"time"
)

// TestInterruptContextCancelsOnShutdownSignals is the regression test for an
// unhandled SIGTERM. The command context must observe both shutdown signals:
// SIGINT is the interactive Ctrl-C, and SIGTERM is what process supervisors,
// container runtimes, and agent harness timeouts send first. Before the fix
// only SIGINT was intercepted, so a SIGTERM terminated pi-worker immediately
// with the Go default behavior, skipping child termination, session directory
// removal, and the JSON result. The signal is sent to this test process, so a
// regression fails loudly: without interception the test binary itself dies.
func TestInterruptContextCancelsOnShutdownSignals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		signal syscall.Signal
	}{
		{name: "SIGINT", signal: syscall.SIGINT},
		{name: "SIGTERM", signal: syscall.SIGTERM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, stop := interruptContext()
			defer stop()

			if err := syscall.Kill(syscall.Getpid(), tc.signal); err != nil {
				t.Fatalf("signal self with %v: %v", tc.signal, err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				t.Fatalf("command context not cancelled by %v", tc.signal)
			}
		})
	}
}

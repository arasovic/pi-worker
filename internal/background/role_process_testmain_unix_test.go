//go:build darwin || linux

package background

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/signal"
	"testing"

	"golang.org/x/sys/unix"
)

// roleProcessExitCanary is a test-only sentinel frame payload that instructs
// the spawned child role process to shut down immediately.
var roleProcessExitCanary = []byte("__role_process_test_exit__")

// checkChildRoleCloexec inspects the CLOEXEC flag on the two file handles
// owned by pipes. It does not create additional os.File wrappers and never
// closes any handle. Returns an error describing the first descriptor whose
// O_CLOEXEC bit is not set.
func checkChildRoleCloexec(pipes *childRolePipes) error {
	for name, fh := range map[string]*os.File{
		"request":  pipes.requestReader,
		"response": pipes.responseWriter,
	} {
		flags, err := unix.FcntlInt(fh.Fd(), unix.F_GETFD, 0)
		if err != nil {
			return fmt.Errorf("%s: FcntlInt(F_GETFD): %w", name, err)
		}
		if flags&unix.FD_CLOEXEC == 0 {
			return fmt.Errorf("%s: FD_CLOEXEC not set", name)
		}
	}
	return nil
}

// TestMain controls child role process testing lifecycle:
// when invoked with an internal role argument it enters that child
// mode; otherwise it delegates to the standard test runner.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 {
		switch os.Args[1] {
		case string(roleSupervisor):
			pipes, err := openChildRolePipes()
			if err != nil {
				fmt.Fprintf(os.Stderr, "openChildRolePipes: %v\n", err)
				os.Exit(91)
			}
			// The flag is set by openChildRolePipes after this exec,
			// not inherited through exec. Verify CLOEXEC on the
			// existing handles via F_GETFD.
			if err := checkChildRoleCloexec(pipes); err != nil {
				fmt.Fprintf(os.Stderr, "checkChildRoleCloexec: %v\n", err)
				os.Exit(96)
			}
			for {
				payload, err := readFrame(pipes.requestReader, privateFrameLimit)
				if err != nil {
					if err == io.EOF {
						pipes.Close()
						os.Exit(0)
					}
					fmt.Fprintf(os.Stderr, "readFrame: %v\n", err)
					os.Exit(92)
				}
				if bytes.Equal(payload, roleProcessExitCanary) {
					pipes.Close()
					os.Exit(95)
				}
				if err := writeFrame(pipes.responseWriter, payload, privateFrameLimit); err != nil {
					fmt.Fprintf(os.Stderr, "writeFrame: %v\n", err)
					os.Exit(93)
				}
			}
		case string(roleWorkerHost):
			pipes, err := openChildRolePipes()
			if err != nil {
				fmt.Fprintf(os.Stderr, "openChildRolePipes: %v\n", err)
				os.Exit(94)
			}
			// The flag is set by openChildRolePipes after this exec,
			// not inherited through exec. Verify CLOEXEC on the
			// existing handles via F_GETFD.
			if err := checkChildRoleCloexec(pipes); err != nil {
				fmt.Fprintf(os.Stderr, "checkChildRoleCloexec: %v\n", err)
				os.Exit(96)
			}
			// Test-only helper: intentionally keeps pipes open and never reads fd3 to stress
			// blocked Send cleanup; this is not normal worker-host behavior.
			defer pipes.Close()
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt)
			defer signal.Stop(sigCh)
			<-sigCh
			pipes.Close()
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

//go:build darwin || linux

package background

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// roleProcessExitCanary is a test-only sentinel frame payload that instructs
// the spawned child role process to shut down immediately.
var roleProcessExitCanary = []byte("__role_process_test_exit__")

// checkChildRoleCloexec inspects the CLOEXEC flag on the available role
// pipe handles (supervisor has two, worker-host three). It does not create additional os.File wrappers and never
// closes any handle. Returns an error describing the first descriptor whose
// O_CLOEXEC bit is not set.
func checkChildRoleCloexec(pipes *childRolePipes) error {
	type entry struct {
		name string
		fh   *os.File
	}
	// In order: request, response, ownership (may be nil for supervisors).
	fdchecks := []entry{
		{"request", pipes.requestReader},
		{"response", pipes.responseWriter},
		{"ownership", pipes.ownershipReader},
	}
	for _, e := range fdchecks {
		if e.fh == nil {
			continue
		}
		flags, err := unix.FcntlInt(e.fh.Fd(), unix.F_GETFD, 0)
		if err != nil {
			return fmt.Errorf("%s: FcntlInt(F_GETFD): %w", e.name, err)
		}
		if flags&unix.FD_CLOEXEC == 0 {
			return fmt.Errorf("%s: FD_CLOEXEC not set", e.name)
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
			pipes, err := openChildRolePipes(roleSupervisor)
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

			// The starter-side supervisor start handoff tests set
			// supervisorHandoffChildEnv, switching this child out of the
			// echo loop into the real child-side supervisor start
			// exchange. The variable's value is a Go duration: how long an
			// accepted child keeps running after the exchange before
			// exiting, so the tests can observe the detached supervisor as
			// a live process. Unset or empty keeps the echo behavior every
			// earlier role-process test relies on.
			if hold := os.Getenv(supervisorHandoffChildEnv); hold != "" {
				result, err := receiveSupervisorStart(pipes)
				if err != nil {
					fmt.Fprintf(os.Stderr, "receiveSupervisorStart: %v\n", err)
					os.Exit(90)
				}
				if !result.accepted {
					// A complete rejection already answered the handshake
					// and rolled back every artifact; exit cleanly.
					os.Exit(88)
				}
				holdDuration, perr := time.ParseDuration(hold)
				if perr != nil {
					fmt.Fprintf(os.Stderr, "parse %s hold %q: %v\n", supervisorHandoffChildEnv, hold, perr)
					os.Exit(89)
				}
				// Accepted: behave like a supervisor that lives on after
				// the exchange instead of exiting immediately.
				time.Sleep(holdDuration)
				os.Exit(0)
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
			pipes, err := openChildRolePipes(roleWorkerHost)
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
			// This test role waits only for owner EOF while keeping
			// request/response open. It intentionally never reads fd3.
			defer pipes.Close()
			buf := make([]byte, 1)
			for {
				n, err := pipes.ownershipReader.Read(buf)
				switch {
				case n == 0 && err == io.EOF:
					// Clean EOF reached.
					pipes.Close()
					os.Exit(0)
				case n > 0:
					// Ownership pipe carries no bytes; unexpected byte.
					pipes.Close()
					os.Exit(97)
				default:
					// Other error.
					pipes.Close()
					os.Exit(98)
				}
			}
		}
	}
	os.Exit(m.Run())
}

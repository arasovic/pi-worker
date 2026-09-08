//go:build darwin || linux

package background

import (
	"bytes"
	"encoding/binary"
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

// supervisorHandoffTamperedWorkspace is the workspace value the opt-in
// bind-mismatch test child substitutes for the request's real workspace
// in its accepted reply snapshot: exactly one Snapshot-represented field
// that the starter's strict bind must report, while every other field of
// the reply stays bound to the request and to the exact child PID.
const supervisorHandoffTamperedWorkspace = "tampered-workspace"

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
			// The starter-side bind-mismatch and partial-frame tests set
			// their own environment variables — each value is a Go
			// duration bounding how long the child keeps running after its
			// deliberately damaged reply — switching this child out of the
			// echo loop into one of the two damaged-reply modes below.
			// Each mode never returns. Unset or empty keeps the echo
			// behavior every earlier role-process test relies on.
			if hold := os.Getenv(supervisorHandoffBindMismatchChildEnv); hold != "" {
				runSupervisorHandoffBindMismatchChild(pipes, supervisorHandoffBindMismatchChildEnv, hold)
			}
			if hold := os.Getenv(supervisorHandoffPartialFrameChildEnv); hold != "" {
				runSupervisorHandoffPartialFrameChild(pipes, supervisorHandoffPartialFrameChildEnv, hold)
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

// supervisorHandoffChildAcceptedSnapshot reads exactly one bounded request
// frame from the child request pipe, strictly decodes it, and durably
// prepares the accepted Snapshot for it exactly as receiveSupervisorStart
// does: the same preparation function, the same store and admission gate,
// and the same gate owner identity, which is the exact PID of this child
// process. Both damaged-reply child modes start from this genuine durable
// acceptance, so the replies they put on the wire are strictly valid and
// fully bound to the request and to the exact child PID except for the
// deliberate wire damage each mode applies.
func supervisorHandoffChildAcceptedSnapshot(pipes *childRolePipes) (Snapshot, error) {
	payload, err := readFrame(pipes.requestReader, privateFrameLimit)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read request frame: %w", err)
	}
	req, err := decodeSupervisorStartRequest(payload)
	if err != nil {
		return Snapshot{}, fmt.Errorf("decode request frame: %w", err)
	}
	prep, err := prepareSupervisorStart(req)
	if err != nil {
		return Snapshot{}, fmt.Errorf("prepare acceptance: %w", err)
	}
	return prep.snapshot, nil
}

// holdSupervisorHandoffChild keeps a damaged-reply test child alive for
// the bounded hold duration carried by envVar, then exits 0. The hold is
// the child's own bound: long enough that the starter's failure Close —
// which runs right after the damaged reply is consumed — always kills and
// reaps a still-live child in a passing run, short enough that a child
// orphaned by a hard-killed test binary still terminates on its own. An
// unparsable hold is reported and exits 89, matching the exchange-mode
// child's hold parse failure.
func holdSupervisorHandoffChild(envVar, hold string) {
	holdDuration, perr := time.ParseDuration(hold)
	if perr != nil {
		fmt.Fprintf(os.Stderr, "parse %s hold %q: %v\n", envVar, hold, perr)
		os.Exit(89)
	}
	time.Sleep(holdDuration)
	os.Exit(0)
}

// runSupervisorHandoffBindMismatchChild is the opt-in bind-mismatch
// test-child mode for the starter-side bind-failure handoff test. After
// reading the one request it durably accepts it exactly as the production
// exchange would, then answers with one complete strictly valid accepted
// reply for the same request and the exact child PID in which exactly one
// Snapshot-represented field — Workspace — is deliberately set to
// supervisorHandoffTamperedWorkspace. The starter's strict decode must
// succeed and its bind must fail on that single field. This function never
// returns: after the complete reply is on the wire and both child
// transport ends are closed, the child stays alive for the bounded hold,
// so the starter's bind-failure Close kills and reaps a still-live child.
func runSupervisorHandoffBindMismatchChild(pipes *childRolePipes, envVar, hold string) {
	snap, err := supervisorHandoffChildAcceptedSnapshot(pipes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", envVar, err)
		os.Exit(80)
	}
	// Mismatch exactly one Snapshot-represented field. The durable
	// acceptance on disk is untouched: only the wire copy deviates.
	snap.Workspace = supervisorHandoffTamperedWorkspace
	payload, err := encodeSupervisorStartAccepted(snap)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: encode accepted reply: %v\n", envVar, err)
		os.Exit(80)
	}
	if err := writeFrame(pipes.responseWriter, payload, privateFrameLimit); err != nil {
		fmt.Fprintf(os.Stderr, "%s: write accepted reply frame: %v\n", envVar, err)
		os.Exit(80)
	}
	pipes.Close()
	holdSupervisorHandoffChild(envVar, hold)
}

// runSupervisorHandoffPartialFrameChild is the opt-in partial-frame
// test-child mode for the starter-side read-failure handoff test. After
// reading the one request it durably accepts it exactly as the production
// exchange would, but then puts a damaged frame on the wire: a valid
// length prefix announcing the full accepted reply, followed by only the
// leading half of the reply payload, with the response writer closed so
// the starter reads EOF in the middle of the announced payload — a
// partial-frame read failure, never a decode failure. This function never
// returns: after the partial frame and the child transport ends are
// closed, the child stays alive for the bounded hold, so the starter's
// read-failure Close kills and reaps a still-live child.
func runSupervisorHandoffPartialFrameChild(pipes *childRolePipes, envVar, hold string) {
	snap, err := supervisorHandoffChildAcceptedSnapshot(pipes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", envVar, err)
		os.Exit(81)
	}
	payload, err := encodeSupervisorStartAccepted(snap)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: encode accepted reply: %v\n", envVar, err)
		os.Exit(81)
	}
	// Announce the full reply length but write only the leading half of
	// the payload: the starter must observe a frame cut off mid-payload.
	partial := len(payload) / 2
	if partial <= 0 || partial >= len(payload) {
		fmt.Fprintf(os.Stderr, "%s: reply payload of %d bytes cannot be split in half\n", envVar, len(payload))
		os.Exit(81)
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(payload)))
	if _, err := pipes.responseWriter.Write(prefix[:]); err != nil {
		fmt.Fprintf(os.Stderr, "%s: write frame length prefix: %v\n", envVar, err)
		os.Exit(81)
	}
	if _, err := pipes.responseWriter.Write(payload[:partial]); err != nil {
		fmt.Fprintf(os.Stderr, "%s: write partial reply payload: %v\n", envVar, err)
		os.Exit(81)
	}
	// Closing the response writer sends EOF mid-payload.
	pipes.Close()
	holdSupervisorHandoffChild(envVar, hold)
}

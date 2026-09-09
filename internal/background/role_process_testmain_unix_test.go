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

	"github.com/arasovic/pi-worker/internal/pi"
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

			// The opt-in flow-test driver mode turns this supervisor
			// child into the test stand-in for the future production
			// supervisor dispatch slice: after the real child-side
			// supervisor start exchange it consumes its own prepared
			// admission ticket and runs one real worker-host execution.
			// The environment variables are set by the flow test that
			// spawns the child. Unset or empty keeps the echo behavior
			// every earlier role-process test relies on.
			if outcomePath := os.Getenv(supervisorDriverOutcomeEnv); outcomePath != "" {
				runSupervisorWorkerDriver(pipes, outcomePath, os.Getenv(supervisorDriverHoldEnv))
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
			// The opt-in stuck mode makes this worker-host child ignore
			// everything — its request pipe, its ownership pipe — and live
			// forever, so adapter tests can exercise the bounded fallback
			// kill. The optional pid-file environment records the exact
			// child pid for the spawning test. Unset or empty keeps the
			// ownership-EOF behavior every earlier role-process test
			// relies on.
			if os.Getenv(workerHostStuckEnv) != "" {
				if pidPath := os.Getenv(workerHostStuckPIDFileEnv); pidPath != "" {
					_ = os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", os.Getpid())), 0o600)
				}
				// A bare select {} would trip the runtime deadlock
				// detector and exit by itself, defeating the stuck mode.
				for {
					time.Sleep(24 * time.Hour)
				}
			}
			// The opt-in closed-response mode closes this worker-host
			// child's response writer and then lives forever ignoring
			// everything, exactly like the stuck mode, so adapter tests can
			// observe a response stream at EOF while the child itself is
			// still running. The pid-file environment of the stuck mode
			// records the exact child pid. Unset or empty keeps the
			// ownership-EOF behavior every earlier role-process test relies
			// on.
			if os.Getenv(workerHostClosedResponseEnv) != "" {
				if pidPath := os.Getenv(workerHostStuckPIDFileEnv); pidPath != "" {
					_ = os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", os.Getpid())), 0o600)
				}
				_ = pipes.responseWriter.Close()
				// A bare select {} would trip the runtime deadlock
				// detector and exit by itself, defeating the mode.
				for {
					time.Sleep(24 * time.Hour)
				}
			}
			// The opt-in crash mode exits immediately with code 3 without
			// reading anything, so adapter tests can exercise the
			// host-exited-without-terminal-result path. Unset or empty
			// keeps the ownership-EOF behavior.
			if os.Getenv(workerHostCrashEnv) != "" {
				pipes.Close()
				os.Exit(3)
			}
			// The opt-in execute mode dispatches the real production
			// child-side worker-host handler over these pipes. Unset or
			// empty keeps the ownership-EOF behavior every earlier
			// role-process test relies on.
			if os.Getenv(workerHostExecuteEnv) != "" {
				// The spawning test may bound this child's response write
				// grace through the child's own environment: the child
				// parses it here in TestMain and sets its own
				// workerHostWriteGrace, so real-subprocess tests never
				// mutate the parent process's package global. Unset or
				// empty keeps the production default.
				if grace := os.Getenv(workerHostWriteGraceEnv); grace != "" {
					d, perr := time.ParseDuration(grace)
					if perr != nil || d <= 0 {
						fmt.Fprintf(os.Stderr, "parse %s %q: %v\n", workerHostWriteGraceEnv, grace, perr)
						os.Exit(85)
					}
					workerHostWriteGrace = d
				}
				exchange, err := receiveWorkerHost(pipes)
				if err != nil {
					fmt.Fprintf(os.Stderr, "receiveWorkerHost: %v\n", err)
					os.Exit(71)
				}
				_ = exchange
				os.Exit(0)
			}
			// The opt-in protocol-failure mode scripts one misbehaving
			// host exchange: after reading the one request frame it puts
			// one malformed frame (an unknown kind) on the wire, then one
			// strictly valid completed terminal result frame, and exits 0
			// on its own. Adapter tests use it to prove that a later
			// terminal result never overrides an earlier protocol
			// failure. Unset or empty keeps the ownership-EOF behavior
			// every earlier role-process test relies on.
			if os.Getenv(workerHostProtocolFailureEnv) != "" {
				runWorkerHostProtocolFailureChild(pipes)
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
	// Remove the per-run fakepi build directory (built lazily by the
	// worker-host tests) after the run, mirroring the internal/pi test
	// binary's own fakepi cleanup.
	code := m.Run()
	removeFakePiBuildDir()
	os.Exit(code)
}

// runWorkerHostProtocolFailureChild is the opt-in malformed-then-terminal
// TestMain child mode: it reads exactly one request frame, writes one
// malformed response frame — a strictly valid JSON document whose kind
// is unknown, so the strict parent decode refuses it as a protocol
// failure rather than a transport failure — then one strictly valid
// completed terminal result frame, closes the child transport ends, and
// exits 0. It never returns.
func runWorkerHostProtocolFailureChild(pipes *childRolePipes) {
	if _, err := readFrame(pipes.requestReader, privateFrameLimit); err != nil {
		fmt.Fprintf(os.Stderr, "protocol failure child: read request frame: %v\n", err)
		os.Exit(86)
	}
	malformed := []byte(`{"schemaVersion":1,"kind":"not-a-real-kind"}`)
	if err := writeFrame(pipes.responseWriter, malformed, privateFrameLimit); err != nil {
		fmt.Fprintf(os.Stderr, "protocol failure child: write malformed frame: %v\n", err)
		os.Exit(86)
	}
	terminal, err := encodeWorkerHostResult(pi.WorkerResult{
		Status:      pi.StatusCompleted,
		Explanation: "completed result sent after the malformed frame",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "protocol failure child: encode terminal result: %v\n", err)
		os.Exit(86)
	}
	if err := writeFrame(pipes.responseWriter, terminal, privateFrameLimit); err != nil {
		fmt.Fprintf(os.Stderr, "protocol failure child: write terminal frame: %v\n", err)
		os.Exit(86)
	}
	pipes.Close()
	os.Exit(0)
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

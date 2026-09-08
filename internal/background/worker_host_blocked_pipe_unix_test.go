//go:build darwin || linux

package background

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// waitRoleProcessExit waits up to bound for the role process to exit on
// its own and fails the test when it does not. The caller's cleanup
// kill is the only kill on the failure path; a passing test never
// kills and never reaps early.
func waitRoleProcessExit(t *testing.T, p *roleProcess, bound time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- p.Wait() }()
	select {
	case <-done:
	case <-time.After(bound):
		t.Fatalf("role process %d did not exit on its own within %s", p.cmd.Process.Pid, bound)
	}
	if p.cmd.ProcessState == nil || !p.cmd.ProcessState.Exited() {
		t.Fatalf("role process %d state = %v, want exited", p.cmd.Process.Pid, p.cmd.ProcessState)
	}
}

// TestWorkerHostSpawnedChildBlockedTerminalWriteSelfExitsWithinGrace is
// the real-subprocess regression for the child transport cleanup
// contract: a spawned host runs through ExtraFiles ->
// openChildRolePipes -> receiveWorkerHost while the parent leaves the
// response reader open and unread, and the run's terminal result is
// larger than the pipe capacity (but far below the frame limit), so the
// terminal write blocks. The parent closes only ownership — never
// draining the response pipe, never killing — and the host must exit on
// its own within its child-side write grace, configured through the
// child's TestMain mode rather than the parent's package global. With
// blocking inherited descriptors whose deadlines are unsupported, the
// blocked write pinned the host until the parent's fallback kill.
func TestWorkerHostSpawnedChildBlockedTerminalWriteSelfExitsWithinGrace(t *testing.T) {
	t.Setenv(workerHostExecuteEnv, "1")
	// The child parses this in TestMain and shortens its own write
	// grace; the parent's package global stays untouched.
	t.Setenv(workerHostWriteGraceEnv, "1s")

	// Size the terminal explanation above the response pipe capacity.
	// On Linux the capacity of a default pipe is measured with
	// F_GETPIPE_SZ on a probe pipe (startRoleProcess pipes are equally
	// default pipes); elsewhere a fixed size far above the default
	// capacity is used.
	probeR, probeW, err := os.Pipe()
	if err != nil {
		t.Fatalf("create capacity probe pipe: %v", err)
	}
	capacity := workerHostPipeCapacityBytes(t, probeR)
	probeR.Close()
	probeW.Close()
	textSize := 256 << 10
	if capacity > 0 {
		textSize = 2*capacity + 8<<10
	}
	if textSize > privateFrameLimit/4 {
		t.Fatalf("terminal payload size %d approaches the private frame limit", textSize)
	}
	big := strings.Repeat("x", textSize)

	pidPath := filepath.Join(t.TempDir(), "fakepi.pid")
	setupFakePiEnv(t, happyPathScript(big))
	t.Setenv("FAKEPI_PIDFILE", pidPath)

	p, err := startRoleProcess(testExe(t), roleWorkerHost)
	if err != nil {
		t.Fatalf("startRoleProcess: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	req := validWorkerHostRequest()
	req.workspace = t.TempDir()
	req.piExecutable = fakePiBin(t)
	payload, err := encodeWorkerHostRequest(req)
	if err != nil {
		t.Fatalf("encode worker host request: %v", err)
	}
	if err := p.Send(payload); err != nil {
		t.Fatalf("send request frame: %v", err)
	}
	if err := p.CloseRequest(); err != nil {
		t.Logf("CloseRequest: %v", err)
	}

	// Wait until the run completed and the host cleaned Pi up: the host
	// is then blocked writing the oversized terminal frame into the
	// unread response pipe.
	piPID := readPIDFile(t, pidPath)
	waitProcessGone(t, piPID)

	// Close only ownership. The response reader stays open and unread,
	// and the host is never killed and never drained by the parent.
	if err := p.CloseOwnership(); err != nil {
		t.Fatalf("close ownership writer: %v", err)
	}

	// The host must exit on its own within its child-side write grace:
	// the pollable response descriptor's absolute write deadline fails
	// the blocked write and the host returns.
	waitRoleProcessExit(t, p, 20*time.Second)
	// Exit code 71 is the execute-mode child's own receiveWorkerHost
	// failure exit; a force-killed host would report a signal, never
	// this voluntary code.
	if code := p.cmd.ProcessState.ExitCode(); code != 71 {
		t.Fatalf("host exit code = %d, want its own receiveWorkerHost failure exit 71 (not a kill)", code)
	}
}

// TestWorkerHostSpawnedChildPartialRequestOwnerLossSelfExits is the
// real-subprocess regression for the request read: a spawned host runs
// through ExtraFiles -> openChildRolePipes -> receiveWorkerHost, the
// parent sends a frame length prefix announcing a payload far larger
// than the bytes actually sent, and leaves the request writer open, so
// the host's request read can never complete on its own. Closing only
// ownership must cancel the exchange and expire the host's request-read
// deadline: the pending read returns, the host answers one terminal
// frame and exits on its own — and no Pi is ever launched. With a
// blocking inherited request descriptor, owner loss could not interrupt
// the partial read and pinned the host until a parent kill.
func TestWorkerHostSpawnedChildPartialRequestOwnerLossSelfExits(t *testing.T) {
	logPath := setupFakePiEnv(t, happyPathScript("never"))
	pidPath := filepath.Join(t.TempDir(), "fakepi.pid")
	t.Setenv("FAKEPI_PIDFILE", pidPath)
	t.Setenv(workerHostExecuteEnv, "1")

	p, err := startRoleProcess(testExe(t), roleWorkerHost)
	if err != nil {
		t.Fatalf("startRoleProcess: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	// Announce a payload below the private frame limit but far beyond
	// the pipe capacity and the bytes sent: the request read stays
	// pending until owner loss expires it. The request writer stays
	// open — no EOF can ever end the read.
	const announced = 1 << 20 // 1 MiB, well below the 64 MiB frame limit
	partial := make([]byte, 4+4096)
	binary.BigEndian.PutUint32(partial[:4], announced)
	for i := 4; i < len(partial); i++ {
		partial[i] = 'p'
	}
	if _, err := p.requestWriter.Write(partial); err != nil {
		t.Fatalf("write partial request frame: %v", err)
	}

	// Close only ownership: the host must cancel the pending request
	// read and exit on its own.
	if err := p.CloseOwnership(); err != nil {
		t.Fatalf("close ownership writer: %v", err)
	}

	waitRoleProcessExit(t, p, 20*time.Second)
	if code := p.cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("host exit code = %d, want voluntary exit 0 after the cancelled partial request", code)
	}

	// No Pi was ever launched: no request log and no pid file exist.
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("request log %s exists after a partial request: Pi must never launch", logPath)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("fakepi pid file %s exists after a partial request: Pi must never launch", pidPath)
	}
}

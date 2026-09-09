//go:build darwin || linux

package background

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/testutil/fakepi/script"
)

// workerHostRunTimeout bounds one request's executionTimeout in the
// in-process handler tests: long enough for a correct run, short enough
// that a hung run fails fast.
const workerHostRunTimeout = 30 * time.Second

// readWorkerHostTerminalFrame reads response frames until the terminal
// result frame, skipping process-start notifications, and returns it.
func readWorkerHostTerminalFrame(t *testing.T, fx *workerHostFixture) workerHostResponse {
	t.Helper()
	for {
		frame := readWorkerHostFrame(t, fx)
		switch frame.kind {
		case workerHostFrameProcessStart:
			continue
		case workerHostFrameResult:
			return frame
		default:
			t.Fatalf("unexpected frame kind %q", frame.kind)
		}
	}
}

// TestWorkerHostChildCompletesRunAcrossRequestEOF runs one full handler
// exchange over real pipes: the request writer is closed right after the
// complete request, and the run must still complete — request EOF after
// the complete request does not cancel it; only ownership EOF does. The
// parent observes exactly one process-start notification followed by one
// terminal completed result whose fields are preserved.
func TestWorkerHostChildCompletesRunAcrossRequestEOF(t *testing.T) {
	// Warm gopsutil's one-time probe before the baseline: that setup is
	// deliberately outside the measured exchange below.
	warmUpGopsutilSelfProbe(t)
	fdsBefore := countFDs(t)
	gosBefore := runtime.NumGoroutine()

	setupFakePiEnv(t, happyPathScript("host answer"))
	fx := newWorkerHostFixture(t)
	req := validWorkerHostRequest()
	req.workspace = t.TempDir()
	req.piExecutable = fakePiBin(t)
	sendWorkerHostRequest(t, fx, req)

	// Close the request writer immediately after the complete request:
	// the request EOF must not cancel the run.
	if err := fx.requestWriter.Close(); err != nil {
		t.Fatalf("close request writer: %v", err)
	}
	fx.requestWriter = nil

	done := startWorkerHostChild(fx)

	start := readWorkerHostFrame(t, fx)
	if start.kind != workerHostFrameProcessStart || start.pid <= 0 || start.workerID != req.workerID {
		t.Fatalf("first frame = %+v, want process-start for worker %d", start, req.workerID)
	}
	final := readWorkerHostFrame(t, fx)
	if final.kind != workerHostFrameResult {
		t.Fatalf("second frame kind = %q, want terminal result", final.kind)
	}
	if final.result.Status != pi.StatusCompleted || final.result.Explanation != "host answer" {
		t.Fatalf("terminal result = %+v, want completed with the fake Pi answer", final.result)
	}
	if final.result.Model != req.model {
		t.Fatalf("result model = %q, want %q", final.result.Model, req.model)
	}

	out := waitWorkerHostChild(t, done, 15*time.Second)
	if out.err != nil {
		t.Fatalf("receiveWorkerHost: %v", out.err)
	}
	if out.exchange.request != req {
		t.Fatalf("exchange request mismatch:\n got %+v\nwant %+v", out.exchange.request, req)
	}
	if out.exchange.result.Status != pi.StatusCompleted {
		t.Fatalf("exchange result = %+v", out.exchange.result)
	}
	// The fixture parent ends are closed only now — after the handler
	// finished and released its own pipes — so the leak check sees no
	// descriptor the fixture itself still owns.
	fx.close()
	requireNoWorkerHostLeak(t, fdsBefore, gosBefore)
}

// TestWorkerHostChildRetryNotificationsPreserveAllPIDs runs a script
// whose first catalog request fails transiently, so the worker restarts
// Pi: the handler must forward both process-start identities in launch
// order before the terminal result, and the terminal result must carry
// the retry warning.
func TestWorkerHostChildRetryNotificationsPreserveAllPIDs(t *testing.T) {
	cfg := happyPathScript("retry answer")
	cfg.TriggerSequences = map[string][][]script.Step{
		"get_available_models": {
			{{Response: &script.Response{Success: false, Error: "temporary catalog outage"}}},
			{{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}}},
		},
	}
	logPath := setupFakePiEnv(t, cfg)
	fx := newWorkerHostFixture(t)
	req := validWorkerHostRequest()
	req.workspace = t.TempDir()
	req.piExecutable = fakePiBin(t)
	sendWorkerHostRequest(t, fx, req)
	done := startWorkerHostChild(fx)

	var pids []int
	for {
		frame := readWorkerHostFrame(t, fx)
		switch frame.kind {
		case workerHostFrameProcessStart:
			pids = append(pids, frame.pid)
		case workerHostFrameResult:
			if len(pids) != 2 || pids[0] == 0 || pids[1] == 0 || pids[0] == pids[1] {
				t.Fatalf("process-start identities = %v, want two distinct positive pids", pids)
			}
			if frame.result.Status != pi.StatusCompleted || frame.result.Explanation != "retry answer" {
				t.Fatalf("terminal result = %+v, want completed after retry", frame.result)
			}
			if frame.result.Warning != "startup succeeded on attempt 2/3 after unavailable startup failure" {
				t.Fatalf("warning = %q, want the startup retry warning", frame.result.Warning)
			}
			if out := waitWorkerHostChild(t, done, 15*time.Second); out.err != nil {
				t.Fatalf("receiveWorkerHost: %v", out.err)
			}
			types := waitRequestLog(t, logPath, 6)
			if countStrings(types, "get_available_models") != 2 || countStrings(types, "prompt") != 1 {
				t.Fatalf("request log = %v, want two catalog requests and one prompt", types)
			}
			return
		default:
			t.Fatalf("unexpected frame kind %q", frame.kind)
		}
	}
}

// TestWorkerHostChildMalformedRequestNeverLaunchesPi sends a frame that
// is not a strict request document: the handler must answer with one
// terminal failed frame and never launch Pi — no fakepi process, no
// request log, no pid file.
func TestWorkerHostChildMalformedRequestNeverLaunchesPi(t *testing.T) {
	logPath := setupFakePiEnv(t, happyPathScript("never"))
	pidPath := filepath.Join(t.TempDir(), "fakepi.pid")
	t.Setenv("FAKEPI_PIDFILE", pidPath)

	fx := newWorkerHostFixture(t)
	if err := writeFrame(fx.requestWriter, []byte("this is not a worker host request"), privateFrameLimit); err != nil {
		t.Fatalf("write malformed frame: %v", err)
	}
	done := startWorkerHostChild(fx)

	final := readWorkerHostFrame(t, fx)
	if final.kind != workerHostFrameResult || final.result.Status != pi.StatusFailed {
		t.Fatalf("terminal frame = %+v, want failed result", final)
	}
	if !strings.Contains(final.result.Error, "decode request frame") {
		t.Fatalf("error %q does not report the strict decode failure", final.result.Error)
	}
	out := waitWorkerHostChild(t, done, 15*time.Second)
	if out.err != nil {
		t.Fatalf("receiveWorkerHost: %v", out.err)
	}
	if out.exchange.request != (workerHostRequest{}) {
		t.Fatalf("malformed exchange decoded a request: %+v", out.exchange.request)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("request log exists after a malformed request: Pi must never launch")
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("fakepi pid file exists after a malformed request: Pi must never launch")
	}
}

// TestWorkerHostChildOversizedRequestRejected sends a frame whose
// announced length exceeds the private frame limit: the handler must
// reject it before allocation and answer with one terminal failed frame,
// never launching Pi.
func TestWorkerHostChildOversizedRequestRejected(t *testing.T) {
	setupFakePiEnv(t, happyPathScript("never"))
	fx := newWorkerHostFixture(t)
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(privateFrameLimit)+1)
	if _, err := fx.requestWriter.Write(prefix[:]); err != nil {
		t.Fatalf("write oversized length prefix: %v", err)
	}
	done := startWorkerHostChild(fx)

	final := readWorkerHostFrame(t, fx)
	if final.kind != workerHostFrameResult || final.result.Status != pi.StatusFailed {
		t.Fatalf("terminal frame = %+v, want failed result", final)
	}
	if !strings.Contains(final.result.Error, "read request frame") {
		t.Fatalf("error %q does not report the frame read failure", final.result.Error)
	}
	out := waitWorkerHostChild(t, done, 15*time.Second)
	if out.err != nil {
		t.Fatalf("receiveWorkerHost: %v", out.err)
	}
}

// TestWorkerHostChildRequestEOFBeforeRequest sends no request at all and
// closes the request writer: the handler must answer with one terminal
// failed frame and never launch Pi. Request EOF cancels nothing because
// there is no complete request to run.
func TestWorkerHostChildRequestEOFBeforeRequest(t *testing.T) {
	logPath := setupFakePiEnv(t, happyPathScript("never"))
	fx := newWorkerHostFixture(t)
	if err := fx.requestWriter.Close(); err != nil {
		t.Fatalf("close request writer: %v", err)
	}
	fx.requestWriter = nil
	done := startWorkerHostChild(fx)

	final := readWorkerHostFrame(t, fx)
	if final.kind != workerHostFrameResult || final.result.Status != pi.StatusFailed {
		t.Fatalf("terminal frame = %+v, want failed result", final)
	}
	if !strings.Contains(final.result.Error, "read request frame") {
		t.Fatalf("error %q does not report the request EOF", final.result.Error)
	}
	out := waitWorkerHostChild(t, done, 15*time.Second)
	if out.err != nil {
		t.Fatalf("receiveWorkerHost: %v", out.err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("request log exists after an empty request: Pi must never launch")
	}
}

// TestWorkerHostChildExecutionTimeoutReportsTimedOut sends a request
// whose execution timeout expires while the run is in flight: the
// handler must stop Pi, clean it up, and report one terminal timed-out
// result.
func TestWorkerHostChildExecutionTimeoutReportsTimedOut(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "fakepi.pid")
	setupFakePiEnv(t, stuckPathScript())
	t.Setenv("FAKEPI_PIDFILE", pidPath)

	fx := newWorkerHostFixture(t)
	req := validWorkerHostRequest()
	req.workspace = t.TempDir()
	req.piExecutable = fakePiBin(t)
	req.executionTimeout = 500 * time.Millisecond
	sendWorkerHostRequest(t, fx, req)
	done := startWorkerHostChild(fx)

	piPID := readPIDFile(t, pidPath)
	if !processAlive(piPID) {
		t.Fatalf("fakepi %d is not alive after start", piPID)
	}

	var frames []workerHostResponse
	for {
		frame := readWorkerHostFrame(t, fx)
		frames = append(frames, frame)
		if frame.kind == workerHostFrameResult {
			break
		}
	}
	final := frames[len(frames)-1]
	if final.result.Status != pi.StatusTimedOut {
		t.Fatalf("terminal result = %+v, want timed out", final.result)
	}
	if !strings.Contains(final.result.Error, "timed out") {
		t.Fatalf("error %q does not report the timeout", final.result.Error)
	}
	out := waitWorkerHostChild(t, done, 15*time.Second)
	if out.err != nil {
		t.Fatalf("receiveWorkerHost: %v", out.err)
	}
	if out.exchange.result.Status != pi.StatusTimedOut {
		t.Fatalf("exchange result = %+v", out.exchange.result)
	}
	waitProcessGone(t, piPID)
}

// TestWorkerHostChildOwnershipEOFCancelsRun closes the ownership writer
// while the run is in flight: the handler must cancel the run, clean Pi
// up, and report one terminal cancelled result, then return cleanly.
func TestWorkerHostChildOwnershipEOFCancelsRun(t *testing.T) {
	// Warm gopsutil's one-time probe before the baseline: that setup is
	// deliberately outside the measured exchange below.
	warmUpGopsutilSelfProbe(t)
	fdsBefore := countFDs(t)
	gosBefore := runtime.NumGoroutine()
	pidPath := filepath.Join(t.TempDir(), "fakepi.pid")
	setupFakePiEnv(t, stuckPathScript())
	t.Setenv("FAKEPI_PIDFILE", pidPath)

	fx := newWorkerHostFixture(t)
	req := validWorkerHostRequest()
	req.workspace = t.TempDir()
	req.piExecutable = fakePiBin(t)
	req.executionTimeout = 30 * time.Second
	sendWorkerHostRequest(t, fx, req)
	done := startWorkerHostChild(fx)

	piPID := readPIDFile(t, pidPath)
	if !processAlive(piPID) {
		t.Fatalf("fakepi %d is not alive after start", piPID)
	}
	if err := fx.ownershipWriter.Close(); err != nil {
		t.Fatalf("close ownership writer: %v", err)
	}
	fx.ownershipWriter = nil

	final := readWorkerHostTerminalFrame(t, fx)
	if final.result.Status != pi.StatusCancelled {
		t.Fatalf("terminal frame = %+v, want cancelled result", final)
	}
	out := waitWorkerHostChild(t, done, 15*time.Second)
	if out.err != nil {
		t.Fatalf("receiveWorkerHost: %v", out.err)
	}
	if out.exchange.result.Status != pi.StatusCancelled {
		t.Fatalf("exchange result = %+v", out.exchange.result)
	}
	waitProcessGone(t, piPID)
	// The fixture parent ends are closed only now — after the handler
	// finished its cancellation cleanup — so the leak check sees no
	// descriptor the fixture itself still owns.
	fx.close()
	requireNoWorkerHostLeak(t, fdsBefore, gosBefore)
}

// TestWorkerHostChildOwnershipLostBeforeRequestAnswers closes the
// ownership writer and the request writer before any request is sent
// (the parent died mid-send before delivering anything): the handler
// must answer one terminal frame and return, never launching Pi. The
// frame reports cancelled when the ownership watcher wins the race and
// failed when the request EOF is observed first — both are honest: no
// run ever started.
func TestWorkerHostChildOwnershipLostBeforeRequestAnswers(t *testing.T) {
	logPath := setupFakePiEnv(t, happyPathScript("never"))
	fx := newWorkerHostFixture(t)
	done := startWorkerHostChild(fx)
	if err := fx.ownershipWriter.Close(); err != nil {
		t.Fatalf("close ownership writer: %v", err)
	}
	fx.ownershipWriter = nil
	if err := fx.requestWriter.Close(); err != nil {
		t.Fatalf("close request writer: %v", err)
	}
	fx.requestWriter = nil

	final := readWorkerHostTerminalFrame(t, fx)
	if final.result.Status != pi.StatusCancelled && final.result.Status != pi.StatusFailed {
		t.Fatalf("terminal frame = %+v, want cancelled or failed result", final)
	}
	if !strings.Contains(final.result.Error, "read request frame") {
		t.Fatalf("error %q does not report the request read outcome", final.result.Error)
	}
	out := waitWorkerHostChild(t, done, 15*time.Second)
	if out.err != nil {
		t.Fatalf("receiveWorkerHost: %v", out.err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("request log exists: Pi must never launch after ownership loss before the request")
	}
}

// TestWorkerHostChildBrokenResponseTransportDoesNotPinCleanup breaks the
// response transport only after the run is established, then closes
// ownership: the handler must cancel the run, clean Pi up, and return
// the transport error instead of pinning cleanup on the broken channel.
// The barrier is deterministic — the process-start notification is read
// on the intact transport, the recorded pid file matches the notified
// identity, and the process is alive while ownership is still open, so
// nothing can kill Pi before the transport is broken. No assertion ever
// races a Pi that may die before it records its own pid.
func TestWorkerHostChildBrokenResponseTransportDoesNotPinCleanup(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "fakepi.pid")
	setupFakePiEnv(t, stuckPathScript())
	t.Setenv("FAKEPI_PIDFILE", pidPath)

	fx := newWorkerHostFixture(t)
	req := validWorkerHostRequest()
	req.workspace = t.TempDir()
	req.piExecutable = fakePiBin(t)
	sendWorkerHostRequest(t, fx, req)
	done := startWorkerHostChild(fx)

	// Deterministic barrier: the handler launched fakepi, forwarded its
	// identity over the intact transport, and the run is in flight (the
	// script never settles). The recorded pid file must match the
	// notified identity, and the process must be alive: only after this
	// may the transport be touched.
	start := readWorkerHostFrame(t, fx)
	if start.kind != workerHostFrameProcessStart || start.pid <= 0 || start.workerID != req.workerID {
		t.Fatalf("first frame = %+v, want process-start for worker %d", start, req.workerID)
	}
	piPID := readPIDFile(t, pidPath)
	if piPID != start.pid {
		t.Fatalf("recorded fakepi pid = %d, want the notified identity %d", piPID, start.pid)
	}
	if !processAlive(piPID) {
		t.Fatalf("fakepi %d is not alive while the run is in flight", piPID)
	}

	// Break the response transport, then close ownership. The run is
	// stuck in flight, so the handler's next response write can only be
	// the terminal frame after the ownership EOF cancels the run and Pi
	// is cleaned up; the closed read end makes that write fail with the
	// transport error instead of pinning the handler.
	if err := fx.responseReader.Close(); err != nil {
		t.Fatalf("close response reader: %v", err)
	}
	fx.responseReader = nil
	if err := fx.ownershipWriter.Close(); err != nil {
		t.Fatalf("close ownership writer: %v", err)
	}
	fx.ownershipWriter = nil

	out := waitWorkerHostChild(t, done, 15*time.Second)
	if out.err == nil {
		t.Fatal("receiveWorkerHost returned nil error on a broken response transport")
	}
	if !strings.Contains(out.err.Error(), "write terminal result frame") {
		t.Fatalf("error %q does not report the broken response transport write", out.err)
	}
	if out.exchange.result.Status != pi.StatusCancelled {
		t.Fatalf("exchange result = %+v, want the cancelled run whose terminal frame could not be delivered", out.exchange.result)
	}
	waitProcessGone(t, piPID)
}

// TestWorkerHostChildBlockedResponseWriteDoesNotPinAfterOwnerLoss runs a
// run whose terminal result frame is far larger than the pipe capacity
// while the parent never reads: the terminal write blocks. Closing the
// ownership writer must not pin the handler: the bounded write deadline
// (shortened here) fails the blocked write, the handler returns, and Pi
// was already cleaned up before the terminal frame was attempted.
func TestWorkerHostChildBlockedResponseWriteDoesNotPinAfterOwnerLoss(t *testing.T) {
	oldGrace := workerHostWriteGrace
	workerHostWriteGrace = 600 * time.Millisecond
	t.Cleanup(func() { workerHostWriteGrace = oldGrace })

	big := strings.Repeat("x", 200<<10)
	pidPath := filepath.Join(t.TempDir(), "fakepi.pid")
	setupFakePiEnv(t, happyPathScript(big))
	t.Setenv("FAKEPI_PIDFILE", pidPath)

	fx := newWorkerHostFixture(t)
	req := validWorkerHostRequest()
	req.workspace = t.TempDir()
	req.piExecutable = fakePiBin(t)
	sendWorkerHostRequest(t, fx, req)
	done := startWorkerHostChild(fx)

	// Wait for the run and its Pi cleanup to finish: the handler is then
	// blocked writing the oversized terminal frame into the unread pipe.
	piPID := readPIDFile(t, pidPath)
	waitProcessGone(t, piPID)

	// Ownership loss while the terminal write is blocked: the bounded
	// write deadline must fail the blocked write and let the handler
	// return instead of pinning cleanup forever.
	if err := fx.ownershipWriter.Close(); err != nil {
		t.Fatalf("close ownership writer: %v", err)
	}
	fx.ownershipWriter = nil

	out := waitWorkerHostChild(t, done, 5*time.Second)
	if out.err == nil {
		t.Fatal("receiveWorkerHost returned nil despite the blocked terminal write failing")
	}
	if !strings.Contains(out.err.Error(), "write terminal result frame") {
		t.Fatalf("error %q does not report the terminal frame write failure", out.err)
	}
	// Nothing was left running or blocked: the stream holds only the
	// partial terminal frame bytes the blocked write managed to buffer,
	// followed by clean EOF once the handler closed its writer.
	if err := fx.responseReader.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	drained := 0
	buf := make([]byte, 4096)
	for {
		n, err := fx.responseReader.Read(buf)
		drained += n
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("drain response stream after handler return: %v", err)
		}
	}
	if drained == 0 {
		t.Fatal("response stream carried no partial terminal frame bytes before EOF")
	}
}

// TestWorkerHostSpawnedChildExecutesAndExitsCleanly is the cross-process
// wiring test for the host-execute TestMain mode: a real spawned
// roleWorkerHost child runs the production handler, receives the request
// over the role pipes, forwards the process identity, sends the terminal
// result, and exits voluntarily with code 0 — with no payload anywhere
// in argv, environment, or stdio.
func TestWorkerHostSpawnedChildExecutesAndExitsCleanly(t *testing.T) {
	t.Setenv(workerHostExecuteEnv, "1")
	setupFakePiEnv(t, happyPathScript("spawned answer"))

	exe := testExe(t)
	p, err := startRoleProcess(exe, roleWorkerHost)
	if err != nil {
		t.Fatalf("startRoleProcess: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	if got := p.cmd.Args; len(got) != 2 || got[0] != exe || got[1] != string(roleWorkerHost) {
		t.Fatalf("cmd.Args = %q, want [%q, %q]: no payload may appear in argv", got, exe, roleWorkerHost)
	}
	if p.cmd.Env != nil {
		t.Fatalf("cmd.Env = %v, want nil: the child inherits the parent environment untouched", p.cmd.Env)
	}
	if len(p.cmd.ExtraFiles) != 3 {
		t.Fatalf("ExtraFiles length = %d, want 3 (request reader, response writer, ownership reader)", len(p.cmd.ExtraFiles))
	}
	if p.cmd.Stdout != nil || p.cmd.Stderr != nil {
		t.Fatalf("child stdout/stderr are connected: %v/%v, want the null device", p.cmd.Stdout, p.cmd.Stderr)
	}

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
	// Request EOF after the complete request does not cancel the run.
	if err := p.CloseRequest(); err != nil {
		t.Logf("CloseRequest: %v", err)
	}

	startFrame, err := p.Receive()
	if err != nil {
		t.Fatalf("receive process-start frame: %v", err)
	}
	decodedStart, err := decodeWorkerHostResponse(startFrame)
	if err != nil || decodedStart.kind != workerHostFrameProcessStart || decodedStart.pid <= 0 {
		t.Fatalf("process-start frame = %+v, %v", decodedStart, err)
	}
	finalFrame, err := p.Receive()
	if err != nil {
		t.Fatalf("receive terminal frame: %v", err)
	}
	decodedFinal, err := decodeWorkerHostResponse(finalFrame)
	if err != nil || decodedFinal.kind != workerHostFrameResult {
		t.Fatalf("terminal frame decode: %+v, %v", decodedFinal, err)
	}
	if decodedFinal.result.Status != pi.StatusCompleted || decodedFinal.result.Explanation != "spawned answer" {
		t.Fatalf("terminal result = %+v", decodedFinal.result)
	}
	if err := p.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if p.cmd.ProcessState == nil || !p.cmd.ProcessState.Exited() || p.cmd.ProcessState.ExitCode() != 0 {
		t.Fatalf("child state = %v, want voluntary exit code 0", p.cmd.ProcessState)
	}
}

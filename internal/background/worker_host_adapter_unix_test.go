//go:build darwin || linux

package background

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/testutil/fakepi/script"

	"golang.org/x/sys/unix"
)

// startHostExecutingAdapter configures the environment so any
// roleWorkerHost child spawned by the adapter dispatches the real
// production handler, and returns an adapter over the test binary and
// the built fakepi helper.
func startHostExecutingAdapter(t *testing.T) *workerHostAdapter {
	t.Helper()
	t.Setenv(workerHostExecuteEnv, "1")
	return newWorkerHostAdapter(testExe(t), fakePiBin(t))
}

// adapterWorkerRequest returns one runnable worker request for the
// adapter tests.
func adapterWorkerRequest(workspace string) pi.WorkerRequest {
	return pi.WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "run the focused task",
		Workspace: workspace,
		WorkerID:  1,
	}
}

// TestWorkerHostAdapterValidationFailures mirrors the executed worker's
// own pre-launch validation: each failure returns the same status and
// message the worker would, without spawning any host.
func TestWorkerHostAdapterValidationFailures(t *testing.T) {
	a := newWorkerHostAdapter(testExe(t), "/unused/pi")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ws := t.TempDir()

	tests := []struct {
		name    string
		mutate  func(*pi.WorkerRequest)
		wantErr string
	}{
		{"empty model", func(r *pi.WorkerRequest) { r.Model = "" }, "model is required"},
		{"invalid thinking", func(r *pi.WorkerRequest) { r.ThinkingLevel = "turbo" }, "invalid thinking level"},
		{"empty prompt", func(r *pi.WorkerRequest) { r.Prompt = "" }, "prompt is required"},
		{"empty workspace", func(r *pi.WorkerRequest) { r.Workspace = "" }, "workspace is required"},
		{"invalid model selector", func(r *pi.WorkerRequest) { r.Model = "no-slash" }, "invalid model selector"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := adapterWorkerRequest(ws)
			tt.mutate(&req)
			result := a.Run(ctx, req)
			if result.Status != pi.StatusFailed {
				t.Fatalf("result = %+v, want failed", result)
			}
			if !strings.Contains(result.Error, tt.wantErr) {
				t.Fatalf("error %q does not contain %q", result.Error, tt.wantErr)
			}
		})
	}
}

// TestWorkerHostAdapterRejectsUnsupportedControls verifies that non-nil
// controls are explicitly rejected instead of silently ignored.
func TestWorkerHostAdapterRejectsUnsupportedControls(t *testing.T) {
	a := newWorkerHostAdapter(testExe(t), "/unused/pi")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req := adapterWorkerRequest(t.TempDir())
	req.Controls = make(chan pi.WorkerControl)
	result := a.Run(ctx, req)
	if result.Status != pi.StatusFailed {
		t.Fatalf("result = %+v, want explicit failed rejection", result)
	}
	if !strings.Contains(result.Error, "controls are not supported") {
		t.Fatalf("error %q does not name the unsupported controls", result.Error)
	}
}

// TestWorkerHostAdapterRequiresExecutionDeadline verifies that a
// context without a deadline is rejected before any host exists: the
// private worker host needs an execution timeout it can enforce.
func TestWorkerHostAdapterRequiresExecutionDeadline(t *testing.T) {
	a := newWorkerHostAdapter(testExe(t), "/unused/pi")
	result := a.Run(context.Background(), adapterWorkerRequest(t.TempDir()))
	if result.Status != pi.StatusFailed {
		t.Fatalf("result = %+v, want failed", result)
	}
	if !strings.Contains(result.Error, "execution deadline is required") {
		t.Fatalf("error %q does not report the missing deadline", result.Error)
	}
}

// TestWorkerHostAdapterExpiredContextsClassifyWithoutSpawn verifies that
// an already-expired context returns the worker's own timed-out or
// cancelled classification without spawning any host.
func TestWorkerHostAdapterExpiredContextsClassifyWithoutSpawn(t *testing.T) {
	a := newWorkerHostAdapter(testExe(t), "/unused/pi")
	req := adapterWorkerRequest(t.TempDir())

	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if result := a.Run(cancelCtx, req); result.Status != pi.StatusCancelled {
		t.Fatalf("cancelled context result = %+v, want cancelled", result)
	}

	deadlineCtx, deadlineCancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer deadlineCancel()
	time.Sleep(10 * time.Millisecond)
	if result := a.Run(deadlineCtx, req); result.Status != pi.StatusTimedOut {
		t.Fatalf("expired deadline result = %+v, want timed out", result)
	}
}

// TestWorkerHostAdapterOversizedRequestRejectedBeforeSpawn verifies that
// a request whose encoded payload exceeds the private frame limit is
// rejected before any host or Pi process exists.
func TestWorkerHostAdapterOversizedRequestRejectedBeforeSpawn(t *testing.T) {
	a := newWorkerHostAdapter(testExe(t), "/unused/pi")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req := adapterWorkerRequest(t.TempDir())
	req.Prompt = strings.Repeat("p", privateFrameLimit+1)
	result := a.Run(ctx, req)
	if result.Status != pi.StatusFailed {
		t.Fatalf("result = %+v, want failed", result)
	}
	if !strings.Contains(result.Error, "exceeding the private frame limit") {
		t.Fatalf("error %q does not report the frame limit", result.Error)
	}
}

// TestWorkerHostAdapterCompletedRunForwardsProcessIdentity runs one full
// adapter exchange against a real spawned host and fake Pi: the terminal
// result is preserved, the process-start notification reaches the
// observer with the identity the caller assigned, and no goroutine or
// file descriptor survives.
func TestWorkerHostAdapterCompletedRunForwardsProcessIdentity(t *testing.T) {
	fdsBefore := countFDs(t)
	gosBefore := runtime.NumGoroutine()
	a := startHostExecutingAdapter(t)
	setupFakePiEnv(t, happyPathScript("adapter answer"))

	ctx, cancel := context.WithTimeout(context.Background(), workerHostRunTimeout)
	defer cancel()
	var starts []int
	var startIDs []int
	result := a.Run(ctx, pi.WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "run the focused task",
		Workspace: t.TempDir(),
		WorkerID:  4,
		OnProcessStart: func(workerID, pid int) {
			startIDs = append(startIDs, workerID)
			starts = append(starts, pid)
		},
	})

	if result.Status != pi.StatusCompleted || result.Explanation != "adapter answer" {
		t.Fatalf("result = %+v, want completed with the fake Pi answer", result)
	}
	if result.Model != "acme/m-1" {
		t.Fatalf("result model = %q", result.Model)
	}
	if len(starts) != 1 || starts[0] <= 0 {
		t.Fatalf("process starts = %v, want one positive pid", starts)
	}
	if len(startIDs) != 1 || startIDs[0] != 4 {
		t.Fatalf("process-start identities = %v, want the assigned worker id 4", startIDs)
	}
	requireNoWorkerHostLeak(t, fdsBefore, gosBefore)
}

// TestWorkerHostAdapterRetryNotificationsForwardAllPIDs runs a script
// with one transient startup failure: both startup-retry identities must
// reach the observer in launch order, and the terminal result must
// preserve the retry warning.
func TestWorkerHostAdapterRetryNotificationsForwardAllPIDs(t *testing.T) {
	a := startHostExecutingAdapter(t)
	cfg := happyPathScript("retry answer")
	cfg.TriggerSequences = map[string][][]script.Step{
		"get_available_models": {
			{{Response: &script.Response{Success: false, Error: "temporary catalog outage"}}},
			{{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}}},
		},
	}
	setupFakePiEnv(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), workerHostRunTimeout)
	defer cancel()
	var starts []int
	result := a.Run(ctx, pi.WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "run the focused task",
		Workspace: t.TempDir(),
		WorkerID:  1,
		OnProcessStart: func(workerID, pid int) {
			starts = append(starts, pid)
		},
	})

	if result.Status != pi.StatusCompleted {
		t.Fatalf("result = %+v, want completed after retry", result)
	}
	if len(starts) != 2 || starts[0] == 0 || starts[1] == 0 || starts[0] == starts[1] {
		t.Fatalf("process starts = %v, want two distinct positive pids", starts)
	}
	if result.Warning != "startup succeeded on attempt 2/3 after unavailable startup failure" {
		t.Fatalf("warning = %q, want the startup retry warning", result.Warning)
	}
}

// TestWorkerHostAdapterFailedRunPreservesResultFields runs a script
// whose prompt is rejected: the terminal failed result must pass through
// with its fields preserved and no fabricated success.
func TestWorkerHostAdapterFailedRunPreservesResultFields(t *testing.T) {
	a := startHostExecutingAdapter(t)
	cfg := happyPathScript("never")
	cfg.Triggers["prompt"] = []script.Step{{Response: &script.Response{Success: false, Error: "task rejected"}}}
	setupFakePiEnv(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), workerHostRunTimeout)
	defer cancel()
	result := a.Run(ctx, adapterWorkerRequest(t.TempDir()))

	if result.Status != pi.StatusFailed {
		t.Fatalf("result = %+v, want failed", result)
	}
	if !strings.Contains(result.Error, "task rejected") {
		t.Fatalf("error %q does not preserve the task rejection", result.Error)
	}
	if result.Model != "acme/m-1" {
		t.Fatalf("result model = %q", result.Model)
	}
}

// TestWorkerHostAdapterCancellationClosesOwnershipAndDrainsHostResult
// cancels the run while it is in flight: the adapter must close
// ownership first, let the host finish its Pi cleanup and report its own
// cancelled terminal result, and return that result — never a
// force-kill, and never a fabricated completion.
func TestWorkerHostAdapterCancellationClosesOwnershipAndDrainsHostResult(t *testing.T) {
	fdsBefore := countFDs(t)
	gosBefore := runtime.NumGoroutine()
	a := startHostExecutingAdapter(t)
	pidPath := filepath.Join(t.TempDir(), "fakepi.pid")
	setupFakePiEnv(t, stuckPathScript())
	t.Setenv("FAKEPI_PIDFILE", pidPath)

	ctx, cancel := context.WithTimeout(context.Background(), workerHostRunTimeout)
	runDone := make(chan pi.WorkerResult, 1)
	go func() {
		runDone <- a.Run(ctx, adapterWorkerRequest(t.TempDir()))
	}()
	piPID := readPIDFile(t, pidPath)
	if !processAlive(piPID) {
		t.Fatalf("fakepi %d is not alive after start", piPID)
	}
	cancel()

	select {
	case result := <-runDone:
		if result.Status != pi.StatusCancelled {
			t.Fatalf("result = %+v, want cancelled", result)
		}
		if strings.Contains(result.Error, "force-killed") {
			t.Fatalf("result %q claims a force-kill on a host that settled on its own", result.Error)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("adapter Run did not return after cancellation within the bounded wait")
	}
	waitProcessGone(t, piPID)
	requireNoWorkerHostLeak(t, fdsBefore, gosBefore)
}

// TestWorkerHostAdapterTimeoutReportsHostTimedOutResult lets the
// execution deadline expire while the run is in flight: the adapter must
// let the host enforce its own deadline (never closing ownership and
// turning the timeout into a cancellation) and return the host's
// timed-out terminal result.
func TestWorkerHostAdapterTimeoutReportsHostTimedOutResult(t *testing.T) {
	a := startHostExecutingAdapter(t)
	pidPath := filepath.Join(t.TempDir(), "fakepi.pid")
	setupFakePiEnv(t, stuckPathScript())
	t.Setenv("FAKEPI_PIDFILE", pidPath)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	result := a.Run(ctx, adapterWorkerRequest(t.TempDir()))
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Fatalf("Run took %v, want a bounded timeout", elapsed)
	}
	if result.Status != pi.StatusTimedOut {
		t.Fatalf("result = %+v, want timed out", result)
	}
	if !strings.Contains(result.Error, "timed out") {
		t.Fatalf("error %q does not report the timeout", result.Error)
	}
	if strings.Contains(result.Error, "force-killed") {
		t.Fatalf("result %q claims a force-kill on a host that settled on its own", result.Error)
	}
	// The fake Pi was cleaned up by the host after its own timeout.
	piPID := readPIDFile(t, pidPath)
	waitProcessGone(t, piPID)
}

// TestWorkerHostAdapterBoundedFallbackKillReportsUncertainty runs a host
// that ignores ownership EOF entirely: the adapter must close ownership,
// wait only the bounded cleanup window, force-kill the host, and report
// the result with explicit cleanup uncertainty — never a fabricated
// completion.
func TestWorkerHostAdapterBoundedFallbackKillReportsUncertainty(t *testing.T) {
	oldBound := workerHostCleanupBound
	workerHostCleanupBound = 400 * time.Millisecond
	t.Cleanup(func() { workerHostCleanupBound = oldBound })

	a := newWorkerHostAdapter(testExe(t), "/unused/pi")
	t.Setenv(workerHostStuckEnv, "1")
	stuckPIDPath := filepath.Join(t.TempDir(), "stuck-host.pid")
	t.Setenv(workerHostStuckPIDFileEnv, stuckPIDPath)

	ctx, cancel := context.WithTimeout(context.Background(), workerHostRunTimeout)
	runDone := make(chan pi.WorkerResult, 1)
	go func() {
		runDone <- a.Run(ctx, adapterWorkerRequest(t.TempDir()))
	}()
	stuckPID := readPIDFile(t, stuckPIDPath)
	if !processAlive(stuckPID) {
		t.Fatalf("stuck host %d is not alive after start", stuckPID)
	}
	t.Cleanup(func() {
		if processAlive(stuckPID) {
			_ = unix.Kill(stuckPID, unix.SIGKILL)
			waitProcessGone(t, stuckPID)
		}
	})
	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case result := <-runDone:
		if result.Status != pi.StatusCancelled {
			t.Fatalf("result = %+v, want cancelled", result)
		}
		if !strings.Contains(result.Error, "force-killed") || !strings.Contains(result.Error, "uncertain") {
			t.Fatalf("error %q does not report the bounded fallback kill and its cleanup uncertainty", result.Error)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("adapter Run did not return after the bounded fallback within the wait")
	}
	waitProcessGone(t, stuckPID)
}

// TestWorkerHostAdapterCancelDuringBlockedSendIsBounded runs a stuck
// host that never reads with a request larger than the pipe capacity, so
// the adapter's send blocks; cancelling must close ownership, bound the
// send drain, force-kill the stuck host, and report the cancellation
// with explicit cleanup uncertainty — never hanging and never closing
// the host before ownership loss.
func TestWorkerHostAdapterCancelDuringBlockedSendIsBounded(t *testing.T) {
	oldBound := workerHostCleanupBound
	workerHostCleanupBound = 500 * time.Millisecond
	t.Cleanup(func() { workerHostCleanupBound = oldBound })

	a := newWorkerHostAdapter(testExe(t), "/unused/pi")
	t.Setenv(workerHostStuckEnv, "1")
	stuckPIDPath := filepath.Join(t.TempDir(), "stuck-host.pid")
	t.Setenv(workerHostStuckPIDFileEnv, stuckPIDPath)

	ctx, cancel := context.WithTimeout(context.Background(), workerHostRunTimeout)
	req := adapterWorkerRequest(t.TempDir())
	req.Prompt = strings.Repeat("p", 2<<20)
	runDone := make(chan pi.WorkerResult, 1)
	go func() { runDone <- a.Run(ctx, req) }()
	stuckPID := readPIDFile(t, stuckPIDPath)
	if !processAlive(stuckPID) {
		t.Fatalf("stuck host %d is not alive after start", stuckPID)
	}
	t.Cleanup(func() {
		if processAlive(stuckPID) {
			_ = unix.Kill(stuckPID, unix.SIGKILL)
			waitProcessGone(t, stuckPID)
		}
	})
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case result := <-runDone:
		if result.Status != pi.StatusCancelled {
			t.Fatalf("result = %+v, want cancelled", result)
		}
		if !strings.Contains(result.Error, "force-killed") || !strings.Contains(result.Error, "uncertain") {
			t.Fatalf("error %q does not report the bounded fallback kill and its cleanup uncertainty", result.Error)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("adapter Run did not return after the bounded send drain within the wait")
	}
	waitProcessGone(t, stuckPID)
}

// TestWorkerHostAdapterProtocolFailureNotOverriddenByLaterTerminal runs
// a real spawned host that answers one request with one malformed frame
// (an unknown kind) followed by one strictly valid completed terminal
// result frame, then exits on its own. The malformed frame starts the
// protocol-failure drain, and the later completed terminal must not
// override it: the adapter returns the non-success protocol failure
// after the host's own cleanup, with the exact child reaped (no
// force-kill, no leaks). Before the fix the completed terminal frame
// overrode the protocol failure and the adapter reported success.
func TestWorkerHostAdapterProtocolFailureNotOverriddenByLaterTerminal(t *testing.T) {
	fdsBefore := countFDs(t)
	gosBefore := runtime.NumGoroutine()
	a := newWorkerHostAdapter(testExe(t), "/unused/pi")
	t.Setenv(workerHostProtocolFailureEnv, "1")

	ctx, cancel := context.WithTimeout(context.Background(), workerHostRunTimeout)
	defer cancel()
	result := a.Run(ctx, adapterWorkerRequest(t.TempDir()))

	if result.Status == pi.StatusCompleted {
		t.Fatalf("result = %+v: a later completed terminal frame must not override the protocol failure", result)
	}
	if result.Status != pi.StatusError {
		t.Fatalf("result = %+v, want the protocol-failure error status", result)
	}
	if !strings.Contains(result.Error, "decode worker host response frame") {
		t.Fatalf("error %q does not report the malformed response frame", result.Error)
	}
	if strings.Contains(result.Error, "force-killed") {
		t.Fatalf("result %q claims a force-kill on a host that exited on its own", result.Error)
	}
	requireNoWorkerHostLeak(t, fdsBefore, gosBefore)
}

// TestWorkerHostAdapterHostCrashWithoutTerminalResult runs a host that
// dies immediately without reading anything: the adapter must report a
// non-success result stating the abnormal exit and its cleanup
// uncertainty, never implying the task ran or completed.
func TestWorkerHostAdapterHostCrashWithoutTerminalResult(t *testing.T) {
	a := newWorkerHostAdapter(testExe(t), "/unused/pi")
	t.Setenv(workerHostCrashEnv, "1")
	ctx, cancel := context.WithTimeout(context.Background(), workerHostRunTimeout)
	defer cancel()
	result := a.Run(ctx, adapterWorkerRequest(t.TempDir()))
	if result.Status != pi.StatusError {
		t.Fatalf("result = %+v, want error", result)
	}
	if !strings.Contains(result.Error, "exited abnormally") || !strings.Contains(result.Error, "uncertain") {
		t.Fatalf("error %q does not state the abnormal exit and cleanup uncertainty", result.Error)
	}
	if result.Status == pi.StatusCompleted {
		t.Fatal("a host without a terminal result must never imply success")
	}
}

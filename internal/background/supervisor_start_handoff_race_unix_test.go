//go:build darwin || linux

package background

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestStartSupervisorHandoffCancelBeforeDecodeAcceptsAndDetaches fires
// the handshake cancellation at the before-decode linearization point:
// the child's complete accepted reply frame has arrived, but strict
// decoding has not begun. Acceptance wins — the handoff reports
// accepted=true, keeps the accepted snapshot and its run directory, and
// never kills the child. The child writes its reply only after its
// acceptance is durable, so the durable artifacts prove the cancellation
// landed exactly in that window rather than earlier.
func TestStartSupervisorHandoffCancelBeforeDecodeAcceptsAndDetaches(t *testing.T) {
	t.Setenv(supervisorHandoffChildEnv, "1s")
	req, backgroundRoot, admissionRoot := exchangeStartRequest(t)
	var proc *roleProcess
	var pid int
	start, _ := captureHandoffStart(t, &proc, &pid)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	supervisorStartCancelProbe = func(phase supervisorStartCancelPhase) {
		if phase == supervisorStartCancelBeforeDecode {
			cancel()
		}
	}
	defer func() { supervisorStartCancelProbe = nil }()
	fdsBefore := countFDs(t)
	gosBefore := runtime.NumGoroutine()

	result, err := startSupervisorHandoffWithProcess(ctx, testExe(t), req, start)
	if !result.accepted {
		t.Fatal("a cancellation after the complete reply frame was read refused the accepted start")
	}
	if err != nil {
		t.Fatalf("cancellation after a complete reply frame must not disturb the acceptance: %v", err)
	}
	if result.snapshot.RunID != req.runID || result.snapshot.Supervisor.PID != pid {
		t.Fatalf("accepted snapshot not bound to the request and child: runId=%q pid=%d",
			result.snapshot.RunID, result.snapshot.Supervisor.PID)
	}

	// The child's acceptance was durable before the reply frame arrived:
	// the accepted snapshot and its run directory are kept, and no rollback
	// ran.
	runDir := filepath.Join(backgroundRoot, req.runID)
	snapPath := filepath.Join(runDir, "snapshot.json")
	if _, statErr := os.Stat(snapPath); statErr != nil {
		t.Fatalf("accepted snapshot missing: stat %s = %v", snapPath, statErr)
	}
	if _, statErr := os.Stat(runDir); statErr != nil {
		t.Fatalf("accepted run directory missing: stat %s = %v", runDir, statErr)
	}
	if st := readAdmissionState(t, admissionRoot); len(st.Tickets) != len(req.tasks) {
		t.Fatalf("durable tickets = %d, want %d for an accepted run", len(st.Tickets), len(req.tasks))
	}

	// The accepted supervisor was detached, not killed: released handle,
	// live child, voluntary exit.
	assertRoleProcessReleased(t, proc)
	assertRoleChildAlive(t, pid)
	status := reapHandoffChild(t, pid, 4*time.Second)
	assertVoluntaryExit(t, status)
	assertRoleChildGone(t, pid)

	requireNoHandoffLeak(t, fdsBefore, gosBefore)
}

// TestAwaitSupervisorStartReplyFrameOutranksReadyCancel verifies the
// phase-5 linearization directly: when a complete reply frame is already
// waiting and the context is already cancelled before the read select runs,
// the frame wins whichever ready channel the random select picks. Without
// the non-blocking re-read, the cancellation arm would refuse a start the
// frame had already conceded. Run with -count=20 to exercise both arms;
// the loop inside makes a regression fail in a single run too.
func TestAwaitSupervisorStartReplyFrameOutranksReadyCancel(t *testing.T) {
	done := make(chan receiveOutcome, 1)
	want := []byte("one complete reply frame")
	for i := 0; i < 64; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // the cancellation is ready before the select runs
		done <- receiveOutcome{frame: want}

		frame, cancelled, readErr := awaitSupervisorStartReply(ctx, done)
		if cancelled {
			t.Fatalf("iteration %d: a complete reply frame lost to a ready cancellation", i)
		}
		if readErr != nil {
			t.Fatalf("iteration %d: read error %v", i, readErr)
		}
		if string(frame) != string(want) {
			t.Fatalf("iteration %d: frame = %q, want %q", i, frame, want)
		}
	}
}

// TestAwaitSupervisorStartReplyCancelWithoutFrameCancels verifies the
// other half of the phase-5 linearization: a cancellation with no reply
// frame delivered is still a cancelled start.
func TestAwaitSupervisorStartReplyCancelWithoutFrameCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan receiveOutcome, 1)

	frame, cancelled, readErr := awaitSupervisorStartReply(ctx, done)
	if !cancelled {
		t.Fatal("a cancellation with no reply frame did not cancel the start")
	}
	if frame != nil {
		t.Fatalf("cancelled start returned frame %q", frame)
	}
	if readErr != nil {
		t.Fatalf("cancelled start returned read error %v", readErr)
	}
}

// TestStartSupervisorHandoffCancelAfterBindAcceptsAndDetaches fires the
// handshake cancellation at the after-bind linearization point: the
// accepted reply is fully decoded and bound. Acceptance must win — the
// handoff detaches the supervisor and reports accepted=true with no
// error, and the racing cancellation never kills the child.
func TestStartSupervisorHandoffCancelAfterBindAcceptsAndDetaches(t *testing.T) {
	t.Setenv(supervisorHandoffChildEnv, "1s")
	req, _, _ := exchangeStartRequest(t)
	var proc *roleProcess
	var pid int
	start, _ := captureHandoffStart(t, &proc, &pid)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	supervisorStartCancelProbe = func(phase supervisorStartCancelPhase) {
		if phase == supervisorStartCancelAfterBind {
			cancel()
		}
	}
	defer func() { supervisorStartCancelProbe = nil }()
	fdsBefore := countFDs(t)
	gosBefore := runtime.NumGoroutine()

	result, err := startSupervisorHandoffWithProcess(ctx, testExe(t), req, start)
	if err != nil {
		t.Fatalf("cancellation after binding must not disturb the acceptance: %v", err)
	}
	if !result.accepted {
		t.Fatal("cancellation after binding rolled back the acceptance")
	}
	if result.snapshot.RunID != req.runID || result.snapshot.Supervisor.PID != pid {
		t.Fatalf("accepted snapshot not bound to the request and child: runId=%q pid=%d",
			result.snapshot.RunID, result.snapshot.Supervisor.PID)
	}

	// The accepted supervisor was detached, not killed: released handle,
	// live child, voluntary exit.
	assertRoleProcessReleased(t, proc)
	assertRoleChildAlive(t, pid)
	status := reapHandoffChild(t, pid, 4*time.Second)
	assertVoluntaryExit(t, status)
	assertRoleChildGone(t, pid)

	requireNoHandoffLeak(t, fdsBefore, gosBefore)
}

// TestStartSupervisorHandoffCancellationRaceNeverKillsAcceptedSupervisor
// sweeps cancellation timings across the whole handshake — before spawn,
// during the send, during the child's durable preparation, around reply
// delivery, and long after the handshake would have finished — and checks
// the invariant by outcome: an accepted trial carries a snapshot bound to
// the exact child PID and that child exits voluntarily (the cancellation
// never raced Detach into killing it), while a canceled trial reports the
// cancellation and leaves the child reaped. Every trial uses fresh state
// roots; the durable artifacts of killed accepted children stay inside
// their temporary roots, which the test framework removes.
func TestStartSupervisorHandoffCancellationRaceNeverKillsAcceptedSupervisor(t *testing.T) {
	t.Setenv(supervisorHandoffChildEnv, "250ms")
	exe := testExe(t)
	fdsBefore := countFDs(t)
	gosBefore := runtime.NumGoroutine()

	delays := []time.Duration{
		0, 100 * time.Microsecond, 500 * time.Microsecond,
		2 * time.Millisecond, 8 * time.Millisecond, 32 * time.Millisecond,
		2 * time.Second, // long after the handshake finished: acceptance wins
	}
	var acceptedCount, canceledCount int
	for round := 0; round < 2; round++ {
		for _, delay := range delays {
			req, _, _ := exchangeStartRequest(t)
			var proc *roleProcess
			var pid int
			start, _ := captureHandoffStart(t, &proc, &pid)
			ctx, cancel := context.WithCancel(context.Background())
			timer := time.AfterFunc(delay, cancel)
			done := make(chan handoffOutcome, 1)
			go func() {
				result, err := startSupervisorHandoffWithProcess(ctx, exe, req, start)
				done <- handoffOutcome{result: result, err: err}
			}()
			out := waitHandoffOutcome(t, done, 20*time.Second)
			timer.Stop()

			if out.result.accepted {
				acceptedCount++
				if out.err != nil {
					t.Fatalf("accepted race trial returned error %v", out.err)
				}
				if proc == nil || pid == 0 {
					t.Fatal("accepted race trial carries no spawned child")
				}
				if out.result.snapshot.Supervisor.PID != pid {
					t.Fatalf("accepted race trial snapshot pid %d != spawned pid %d",
						out.result.snapshot.Supervisor.PID, pid)
				}
				// The child may already be a zombie when the reap poll
				// starts; kill-0 still succeeds on the unreaped exact PID,
				// and the wait status below proves the child was never
				// killed by the racing cancellation.
				assertRoleChildAlive(t, pid)
				status := reapHandoffChild(t, pid, 3*time.Second)
				assertVoluntaryExit(t, status)
				assertRoleChildGone(t, pid)
				continue
			}

			canceledCount++
			if !errors.Is(out.err, context.Canceled) {
				t.Fatalf("canceled race trial returned error %v, want the cancellation", out.err)
			}
			if proc != nil {
				assertRoleChildGone(t, pid)
			}
		}
	}

	if acceptedCount == 0 {
		t.Fatalf("no race trial accepted (canceled %d): the sweep never crossed the acceptance point", canceledCount)
	}
	if canceledCount == 0 {
		t.Fatalf("no race trial canceled (accepted %d): the sweep never crossed a cancellation point", acceptedCount)
	}
	t.Logf("race sweep outcomes: %d accepted, %d canceled", acceptedCount, canceledCount)

	requireNoHandoffLeak(t, fdsBefore, gosBefore)
}

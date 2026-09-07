//go:build darwin || linux

package background

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// startExchangeWithPrepare runs receiveSupervisorStartWithPrepare over the
// fixture's child ends on a fresh goroutine — the startExchange twin for
// tests that substitute a concrete preparation stub — and returns the
// channel that receives the outcome.
func startExchangeWithPrepare(fx *startExchangeFixture, prepare supervisorStartPrepareFunc) <-chan exchangeOutcome {
	done := make(chan exchangeOutcome, 1)
	go func() {
		result, err := receiveSupervisorStartWithPrepare(fx.pipes, prepare)
		done <- exchangeOutcome{result: result, err: err}
	}()
	return done
}

// TestSupervisorStartExchangeInvalidRequestPayloadRejectsOnce sends one
// frame that is not a valid strict start request and verifies the
// protocol-level rejection: the starter receives exactly one complete
// rejection frame naming the decode failure, followed by clean EOF; the
// exchange returns nil with accepted=false and no preparation — nothing
// schedulable — and both child transport ends are closed on return while
// the parent ends stay caller-owned.
func TestSupervisorStartExchangeInvalidRequestPayloadRejectsOnce(t *testing.T) {
	fx := newStartExchangeFixture(t)

	// One frame that is JSON but not a valid start request: the missing
	// schemaVersion fails the strict decode before any preparation.
	if err := writeFrame(fx.requestWriter, []byte(`{}`), privateFrameLimit); err != nil {
		t.Fatalf("write invalid request frame: %v", err)
	}

	done := startExchange(fx)
	_, reply := readStartReply(t, fx)
	if reply.accepted() {
		t.Fatal("invalid request payload produced an accepted reply")
	}
	if !strings.Contains(reply.reason, "schemaVersion must be 1") {
		t.Fatalf("rejection reason %q does not name the decode failure", reply.reason)
	}

	// A clean protocol-level rejection: the complete rejection frame
	// reached the wire, nothing was created, so there is nothing to
	// report.
	out := waitExchange(t, done)
	if out.err != nil {
		t.Fatalf("receiveSupervisorStart: %v", out.err)
	}
	if out.result.accepted || out.result.preparation != nil {
		t.Fatalf("invalid payload returned a schedulable result: accepted=%v preparation=%v", out.result.accepted, out.result.preparation)
	}

	// The child request reader and response writer were closed on return,
	// the one rejection frame is followed by clean EOF, and the parent
	// ends remain caller-owned.
	if fx.pipes.requestReader != nil || fx.pipes.responseWriter != nil {
		t.Fatalf("child transport ends still open after the exchange returned: %+v", fx.pipes)
	}
	if _, err := readFrame(fx.responseReader, privateFrameLimit); !errors.Is(err, io.EOF) {
		t.Fatalf("read after the rejection frame returned %v, want EOF: exactly one reply frame", err)
	}
	if err := fx.requestWriter.Close(); err != nil {
		t.Fatalf("starter request writer is no longer caller-owned: %v", err)
	}
	if err := fx.responseReader.Close(); err != nil {
		t.Fatalf("starter response reader is no longer caller-owned: %v", err)
	}
}

// TestSupervisorStartExchangePrepareNilWithErrorRejectsCleanly injects a
// prepare stub that returns nil together with an error — the branch that
// means nothing was persisted and nothing needs cleanup. The starter
// receives exactly one complete rejection frame whose reason is exactly
// the injected failure, the exchange returns nil error with accepted=false
// and no preparation, no artifact appears in either state root, and the
// child transport ends are closed on return.
func TestSupervisorStartExchangePrepareNilWithErrorRejectsCleanly(t *testing.T) {
	fx := newStartExchangeFixture(t)
	req, backgroundRoot, admissionRoot := exchangeStartRequest(t)
	injected := errors.New("injected prepare failure: nothing persisted")

	sendStartRequest(t, fx, req)
	done := startExchangeWithPrepare(fx, func(supervisorStartRequest) (*supervisorPreparation, error) {
		return nil, injected
	})

	_, reply := readStartReply(t, fx)
	if reply.accepted() {
		t.Fatal("nil-preparation prepare failure produced an accepted reply")
	}
	if reply.reason != injected.Error() {
		t.Fatalf("rejection reason %q does not carry exactly the prepare failure %q", reply.reason, injected)
	}

	// Nothing was persisted, so nothing needs cleanup: a clean rejection.
	out := waitExchange(t, done)
	if out.err != nil {
		t.Fatalf("receiveSupervisorStart: %v", out.err)
	}
	if out.result.accepted || out.result.preparation != nil {
		t.Fatalf("nil-preparation prepare failure returned a schedulable result: accepted=%v preparation=%v", out.result.accepted, out.result.preparation)
	}

	// No artifact in either state root.
	requireDirEmpty(t, backgroundRoot)
	requireDirEmpty(t, admissionRoot)

	// The child request reader and response writer were closed on return,
	// the one rejection frame is followed by clean EOF, and the parent
	// ends remain caller-owned.
	if fx.pipes.requestReader != nil || fx.pipes.responseWriter != nil {
		t.Fatalf("child transport ends still open after the exchange returned: %+v", fx.pipes)
	}
	if _, err := readFrame(fx.responseReader, privateFrameLimit); !errors.Is(err, io.EOF) {
		t.Fatalf("read after the rejection frame returned %v, want EOF: exactly one reply frame", err)
	}
	if err := fx.requestWriter.Close(); err != nil {
		t.Fatalf("starter request writer is no longer caller-owned: %v", err)
	}
	if err := fx.responseReader.Close(); err != nil {
		t.Fatalf("starter response reader is no longer caller-owned: %v", err)
	}
}

// TestSupervisorStartExchangePrepareErrorRollsBackThenRejectsCleanly
// injects a prepare stub that returns a real durable preparation together
// with an error, with the rollback able to complete on its first attempt.
// The exchange must roll the preparation back exactly once — before the
// rejection frame — and reject cleanly: the reason on the wire is exactly
// the injected failure with no cleanup noise, the returned error is nil
// with accepted=false and no preserved preparation, and every durable
// artifact (Snapshot, tickets) is already gone by the time the rejection
// frame is complete.
func TestSupervisorStartExchangePrepareErrorRollsBackThenRejectsCleanly(t *testing.T) {
	fx := newStartExchangeFixture(t)
	req, backgroundRoot, admissionRoot := exchangeStartRequest(t)

	// Build the real durable preparation on the test goroutine, then arm
	// the injected prepare to hand it over together with a failure whose
	// cleanup the handshake must finish.
	prep, err := prepareSupervisorStart(req)
	if err != nil {
		t.Fatalf("prepare the real acceptance state: %v", err)
	}
	if _, err := os.Stat(filepath.Join(backgroundRoot, req.runID, "snapshot.json")); err != nil {
		t.Fatalf("snapshot missing before the exchange: %v", err)
	}
	if st := readAdmissionState(t, admissionRoot); len(st.Tickets) != len(req.tasks) {
		t.Fatalf("durable tickets before the exchange = %d, want %d", len(st.Tickets), len(req.tasks))
	}

	injected := errors.New("injected post-create prepare failure: roll back")
	sendStartRequest(t, fx, req)
	done := startExchangeWithPrepare(fx, func(supervisorStartRequest) (*supervisorPreparation, error) {
		return prep, injected
	})

	_, reply := readStartReply(t, fx)
	if reply.accepted() {
		t.Fatal("prepare failure with a durable preparation produced an accepted reply")
	}
	// The rollback completed before the rejection frame was written, so
	// the reason carries exactly the original failure, never cleanup
	// noise from the rollback.
	if reply.reason != injected.Error() {
		t.Fatalf("rejection reason %q does not carry exactly the prepare failure %q", reply.reason, injected)
	}

	// Rollback precedes the rejection frame: by the time the frame is
	// complete on the starter side, the Snapshot and its run directory
	// are gone and every prepared ticket is cancelled.
	requireDirEmpty(t, backgroundRoot)
	if st := readAdmissionState(t, admissionRoot); len(st.Tickets) != 0 {
		t.Fatalf("tickets left behind after the rolled-back rejection: %+v", st.Tickets)
	}

	// Cleanup completed: the exchange returns nil error with accepted=false
	// and no preserved preparation — nothing schedulable.
	out := waitExchange(t, done)
	if out.err != nil {
		t.Fatalf("clean rejection after a completed rollback returned error: %v", out.err)
	}
	if out.result.accepted || out.result.preparation != nil {
		t.Fatalf("rolled-back rejection returned a schedulable result: accepted=%v preparation=%v", out.result.accepted, out.result.preparation)
	}

	// The child request reader and response writer were closed on return,
	// the one rejection frame is followed by clean EOF, and the parent
	// ends remain caller-owned.
	if fx.pipes.requestReader != nil || fx.pipes.responseWriter != nil {
		t.Fatalf("child transport ends still open after the exchange returned: %+v", fx.pipes)
	}
	if _, err := readFrame(fx.responseReader, privateFrameLimit); !errors.Is(err, io.EOF) {
		t.Fatalf("read after the rejection frame returned %v, want EOF: exactly one reply frame", err)
	}
	if err := fx.requestWriter.Close(); err != nil {
		t.Fatalf("starter request writer is no longer caller-owned: %v", err)
	}
	if err := fx.responseReader.Close(); err != nil {
		t.Fatalf("starter response reader is no longer caller-owned: %v", err)
	}
}

// TestSupervisorStartExchangePrepareErrorRollbackIncompletePreservesPreparation
// injects a prepare stub that returns a real durable preparation together
// with an error, while an extra entry in the run directory forces the
// Snapshot removal inside rollback to fail. The handshake must attempt the
// rollback exactly once — never looping or retrying — and still send the
// rejection: accepted stays false, the rejected reply carries the joined
// history, the returned error preserves the injected cause next to the one
// removal failure, and the preparation itself is preserved on the result
// so the caller owns the unfinished cleanup. Once the obstacle is removed,
// that preserved preparation rolls back to completion.
func TestSupervisorStartExchangePrepareErrorRollbackIncompletePreservesPreparation(t *testing.T) {
	fx := newStartExchangeFixture(t)
	req, backgroundRoot, admissionRoot := exchangeStartRequest(t)

	prep, err := prepareSupervisorStart(req)
	if err != nil {
		t.Fatalf("prepare the real acceptance state: %v", err)
	}

	// An extra entry in the run directory makes strict Store.Remove refuse
	// before deleting anything: the handshake's one rollback attempt
	// cannot remove the Snapshot.
	runDir := filepath.Join(backgroundRoot, req.runID)
	obstacle := filepath.Join(runDir, "unexpected.txt")
	if err := os.WriteFile(obstacle, []byte("unexpected"), 0o600); err != nil {
		t.Fatalf("write unexpected run dir entry: %v", err)
	}

	injected := errors.New("injected post-create prepare failure: cleanup needed")
	sendStartRequest(t, fx, req)
	done := startExchangeWithPrepare(fx, func(supervisorStartRequest) (*supervisorPreparation, error) {
		return prep, injected
	})

	_, reply := readStartReply(t, fx)
	if reply.accepted() {
		t.Fatal("prepare failure with an incomplete rollback produced an accepted reply")
	}
	out := waitExchange(t, done)
	if out.err == nil {
		t.Fatal("incomplete rollback returned no error")
	}
	if !errors.Is(out.err, injected) {
		t.Fatalf("returned error %v does not preserve the original prepare failure", out.err)
	}

	// The handshake attempted the rollback exactly once: the joined error
	// and the rejection reason both carry the injected cause next to a
	// single removal failure, never a second retry of the same failure.
	errText := out.err.Error()
	if strings.Count(errText, "exactly one snapshot.json entry") != 1 {
		t.Fatalf("joined error %q does not show exactly one rollback attempt", errText)
	}
	if !strings.Contains(errText, "remove accepted snapshot") {
		t.Fatalf("joined error %q does not name the failed snapshot removal", errText)
	}
	if reply.reason != errText {
		t.Fatalf("rejection reason %q differs from the returned error %q: the wire must carry the full history", reply.reason, errText)
	}

	// The incomplete rollback preserved the preparation on the result —
	// the caller owns it and it must never be scheduled — while the
	// attempted rollback still cancelled every ticket. The strict removal
	// refusal deleted nothing: Snapshot and obstacle both survive.
	if out.result.accepted {
		t.Fatal("incomplete rollback reported an accepted result")
	}
	if out.result.preparation != prep {
		t.Fatalf("result preparation %p does not preserve the cleanup owner %p", out.result.preparation, prep)
	}
	if _, err := os.Stat(filepath.Join(runDir, "snapshot.json")); err != nil {
		t.Fatalf("snapshot missing after the refused removal: %v", err)
	}
	if _, err := os.Stat(obstacle); err != nil {
		t.Fatalf("unexpected run dir entry missing after the refused removal: %v", err)
	}
	if st := readAdmissionState(t, admissionRoot); len(st.Tickets) != 0 {
		t.Fatalf("tickets left behind by the incomplete rollback: %+v", st.Tickets)
	}

	// The child request reader and response writer were closed on return,
	// the one rejection frame is followed by clean EOF, and the parent
	// ends remain caller-owned.
	if fx.pipes.requestReader != nil || fx.pipes.responseWriter != nil {
		t.Fatalf("child transport ends still open after the exchange returned: %+v", fx.pipes)
	}
	if _, err := readFrame(fx.responseReader, privateFrameLimit); !errors.Is(err, io.EOF) {
		t.Fatalf("read after the rejection frame returned %v, want EOF: exactly one reply frame", err)
	}
	if err := fx.requestWriter.Close(); err != nil {
		t.Fatalf("starter request writer is no longer caller-owned: %v", err)
	}
	if err := fx.responseReader.Close(); err != nil {
		t.Fatalf("starter response reader is no longer caller-owned: %v", err)
	}

	// Once the obstacle is gone, the caller-finished rollback of the
	// preserved preparation completes: the Snapshot is removed and both
	// state roots end clean.
	if err := os.Remove(obstacle); err != nil {
		t.Fatalf("remove unexpected run dir entry: %v", err)
	}
	if err := out.result.preparation.rollback(); err != nil {
		t.Fatalf("later rollback of the preserved preparation: %v", err)
	}
	requireDirEmpty(t, backgroundRoot)
	if st := readAdmissionState(t, admissionRoot); len(st.Tickets) != 0 {
		t.Fatalf("tickets left after the later rollback: %+v", st.Tickets)
	}
}

// TestSupervisorStartExchangeRejectionWriteFailurePreservesOriginalCause
// closes the starter response read end before the rejection is written, so
// the rejection frame cannot reach the wire. The returned error must join
// the rejection write failure under the original decode or prepare cause
// — never replace it — and the result stays non-accepted with nothing
// schedulable.
func TestSupervisorStartExchangeRejectionWriteFailurePreservesOriginalCause(t *testing.T) {
	t.Run("decode failure", func(t *testing.T) {
		fx := newStartExchangeFixture(t)
		if err := writeFrame(fx.requestWriter, []byte(`{}`), privateFrameLimit); err != nil {
			t.Fatalf("write invalid request frame: %v", err)
		}
		// Close the starter response read end before the exchange starts:
		// the rejection write deterministically fails with a broken pipe.
		if err := fx.responseReader.Close(); err != nil {
			t.Fatalf("close starter response read end: %v", err)
		}

		out := waitExchange(t, startExchange(fx))
		if out.err == nil {
			t.Fatal("rejection write failure returned no error")
		}
		errText := out.err.Error()
		if !strings.Contains(errText, "schemaVersion must be 1") {
			t.Fatalf("error %q lost the original decode cause", errText)
		}
		if !strings.Contains(errText, "write rejection frame") || !strings.Contains(errText, "broken pipe") {
			t.Fatalf("error %q does not carry the rejection write failure", errText)
		}
		if out.result.accepted || out.result.preparation != nil {
			t.Fatalf("rejection write failure returned a schedulable result: accepted=%v preparation=%v", out.result.accepted, out.result.preparation)
		}
		if fx.pipes.requestReader != nil || fx.pipes.responseWriter != nil {
			t.Fatalf("child transport ends still open after the exchange returned: %+v", fx.pipes)
		}
		if err := fx.requestWriter.Close(); err != nil {
			t.Fatalf("starter request writer is no longer caller-owned: %v", err)
		}
	})

	t.Run("prepare failure", func(t *testing.T) {
		fx := newStartExchangeFixture(t)
		req := validStartRequest()
		injected := errors.New("injected prepare failure with a broken response pipe")
		sendStartRequest(t, fx, req)
		if err := fx.responseReader.Close(); err != nil {
			t.Fatalf("close starter response read end: %v", err)
		}

		done := startExchangeWithPrepare(fx, func(supervisorStartRequest) (*supervisorPreparation, error) {
			return nil, injected
		})
		out := waitExchange(t, done)
		if out.err == nil {
			t.Fatal("rejection write failure returned no error")
		}
		if !errors.Is(out.err, injected) {
			t.Fatalf("error %v lost the original prepare cause", out.err)
		}
		errText := out.err.Error()
		if !strings.Contains(errText, "write rejection frame") || !strings.Contains(errText, "broken pipe") {
			t.Fatalf("error %q does not carry the rejection write failure", errText)
		}
		if out.result.accepted || out.result.preparation != nil {
			t.Fatalf("rejection write failure returned a schedulable result: accepted=%v preparation=%v", out.result.accepted, out.result.preparation)
		}
		if fx.pipes.requestReader != nil || fx.pipes.responseWriter != nil {
			t.Fatalf("child transport ends still open after the exchange returned: %+v", fx.pipes)
		}
		if err := fx.requestWriter.Close(); err != nil {
			t.Fatalf("starter request writer is no longer caller-owned: %v", err)
		}
	})
}

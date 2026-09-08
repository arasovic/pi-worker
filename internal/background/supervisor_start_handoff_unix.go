//go:build darwin || linux

package background

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"
	"unicode/utf8"

	"github.com/arasovic/pi-worker/internal/run"
	"github.com/arasovic/pi-worker/internal/worktree"
)

// supervisorStartHandoffResult is the outcome of one starter-side
// supervisor start handoff.
//
// accepted reports whether the one reply frame carried an accepted
// snapshot that was strictly decoded and bound to the sent request and to
// the exact spawned supervisor child. snapshot is populated exactly when
// accepted is true. Callers must check accepted before interpreting the
// returned error, matching the existing child-side start result: an
// accepted result may still carry a detach diagnostic and proceeds to
// supervisor scheduling regardless, while a non-accepted result never
// carries a usable snapshot and the child has been closed and reaped.
type supervisorStartHandoffResult struct {
	snapshot Snapshot
	accepted bool
}

// supervisorStartProcessFunc is the concrete process-start function used
// by the starter-side supervisor start handoff. Production always passes
// startRoleProcess; the parameter exists only so tests can observe the
// exact spawned roleProcess and inject start failures, mirroring the
// prepare-function seam of the child-side exchange.
type supervisorStartProcessFunc func(executable string, r role) (*roleProcess, error)

// startSupervisorHandoff runs the production starter-side half of the
// one-shot supervisor acceptance handshake, starting the supervisor child
// through startRoleProcess.
func startSupervisorHandoff(ctx context.Context, executable string, req supervisorStartRequest) (supervisorStartHandoffResult, error) {
	return startSupervisorHandoffWithProcess(ctx, executable, req, startRoleProcess)
}

// supervisorStartCancelPhase names one cancellation linearization point
// of the starter-side handoff. The phase exists only for the private test
// probe below; production code never branches on it.
type supervisorStartCancelPhase int

const (
	// supervisorStartCancelBeforeDecode fires after one complete reply
	// frame has been received and immediately before strict decoding
	// begins. A cancellation observed at this point still closes and
	// reaps the child: the frame has not yet been decoded, bound, or
	// accepted.
	supervisorStartCancelBeforeDecode supervisorStartCancelPhase = iota

	// supervisorStartCancelAfterBind fires after the accepted reply has
	// been strictly decoded and fully bound to the request and to the
	// exact spawned supervisor PID, immediately before Detach. Acceptance
	// is already committed at this point, so a cancellation observed here
	// is ignored: the supervisor is detached and the handoff reports
	// accepted with no error.
	supervisorStartCancelAfterBind
)

// supervisorStartCancelProbe is a private test seam invoked at the two
// cancellation linearization points above. Production leaves it nil;
// tests replace it to fire handshake cancellation — or to sabotage the
// imminent Detach — at exactly those points, and restore it afterwards.
var supervisorStartCancelProbe func(supervisorStartCancelPhase)

// supervisorStartCloseRequestProbe is a private test seam invoked after
// the request frame was sent successfully and immediately before the
// starter request writer is closed. Production leaves it nil; tests use
// it to pre-close the parent request writer behind CloseRequest's back,
// producing a write-success/close-failure handoff against a real child
// that no concrete production seam can create. It runs synchronously on
// the handoff goroutine between Send and CloseRequest, so it races
// neither, and production behavior never branches on it.
var supervisorStartCloseRequestProbe func()

// startSupervisorHandoffWithProcess runs the whole starter-side half of
// the one-shot supervisor acceptance handshake. It encodes and validates
// the request before any process exists, starts exactly one roleSupervisor
// child with only the hidden role token in argv, sends exactly one bounded
// request frame, closes the starter request writer, reads exactly one
// bounded reply frame, decodes it strictly, and binds an accepted reply to
// the request — run id, accepted time (which also stamps the initial
// snapshot update time), workspace, worktree, worker order/projections,
// and execution timeout — and to the exact spawned supervisor PID. An
// encoded request larger than the private frame limit is rejected before
// any context check and before any process exists. A structurally valid
// snapshot for another request is a protocol failure, never an
// acceptance. On acceptance the child is detached; on every other outcome
// it is closed and reaped. All primary and cleanup errors are preserved
// with errors.Join, and a failed close of the starter request writer
// after a successful send is a preserved diagnostic, never a decision:
// the one reply is still read, decoded, and bound, the diagnostic joins
// every later non-accepted error and the accepted return, and an accepted
// result detaches the supervisor and is never closed because of it.
//
// Cancellation and deadlines are respected without ever racing Detach
// against Send, Receive, or Close: the calling goroutine is the only
// actor that performs terminal actions, each blocking Send or Receive
// runs on its own worker goroutine, and Close — which roleProcess is
// designed to run against a blocked Send or Receive — always finishes
// before the worker is drained. Detach runs only after every worker has
// returned, so it races nothing.
//
// The acceptance linearization for cancellation is: cancellation observed
// before process creation, while a Send or Receive is in flight, or after
// a complete reply frame arrives but before strict decoding begins always
// closes and reaps the child and reports non-accepted. Once strict
// decoding begins, the handshake is committed: a complete valid accepted
// reply is decoded and bound, acceptance wins over any later
// cancellation, and the child is detached. A complete rejection returns
// accepted=false with a bounded rejection error and closes and reaps the
// child. After a complete valid accepted reply, Detach is always called;
// a detach diagnostic returns accepted=true with the accepted snapshot
// and the diagnostic, and never rolls back or kills the accepted
// supervisor.
func startSupervisorHandoffWithProcess(ctx context.Context, executable string, req supervisorStartRequest, start supervisorStartProcessFunc) (result supervisorStartHandoffResult, err error) {
	if ctx == nil {
		ctx = context.Background()
	}

	// Phase 1 — encode (and therefore validate) the request before any
	// process or pipe is created. The encoded bytes are the authoritative
	// wire form: the child decodes exactly these bytes, so the accepted
	// reply is later bound against a re-decode of the same payload rather
	// than against in-memory slice shapes.
	payload, encodeErr := encodeSupervisorStartRequest(req)
	if encodeErr != nil {
		return supervisorStartHandoffResult{}, fmt.Errorf("start supervisor handoff: encode request: %w", encodeErr)
	}

	// The encoded payload is exactly the frame the child would receive,
	// so a request larger than the private frame limit is rejected here —
	// before any context check and before any process or pipe exists. No
	// child may be spawned for a request the frame protocol could never
	// deliver and the child-side read would refuse.
	if len(payload) > privateFrameLimit {
		return supervisorStartHandoffResult{}, fmt.Errorf(
			"start supervisor handoff: request payload is %d bytes, exceeding the private frame limit of %d bytes",
			len(payload), privateFrameLimit)
	}

	// Cancellation before process creation leaves nothing to clean up.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return supervisorStartHandoffResult{}, fmt.Errorf("start supervisor handoff: %w", ctxErr)
	}

	// Phase 2 — start exactly one roleSupervisor child. The child sees
	// only the hidden role token in argv and inherits the parent
	// environment; the request travels exclusively over the request pipe.
	proc, startErr := start(executable, roleSupervisor)
	if startErr != nil {
		return supervisorStartHandoffResult{}, fmt.Errorf("start supervisor handoff: start role process: %w", startErr)
	}
	supervisorPID := proc.cmd.Process.Pid

	// Phase 3 — send exactly one request frame. The send runs on a worker
	// goroutine so a child that never reads cannot stall cancellation.
	sendDone := make(chan error, 1)
	go func() { sendDone <- proc.Send(payload) }()
	select {
	case sendErr := <-sendDone:
		if sendErr != nil {
			closeErr := proc.Close()
			return supervisorStartHandoffResult{}, joinSupervisorStartErrors(
				fmt.Errorf("start supervisor handoff: send request frame: %w", sendErr), closeErr)
		}
	case <-ctx.Done():
		// Close kills and reaps the child first, which unblocks the
		// blocked write; only then drain the worker. The worker error is
		// an artifact of this cleanup, so it is not joined.
		closeErr := proc.Close()
		<-sendDone
		return supervisorStartHandoffResult{}, joinSupervisorStartErrors(
			fmt.Errorf("start supervisor handoff: %w", ctx.Err()), closeErr)
	}

	// Phase 4 — close the starter request writer. The exchange child
	// answers one reply without ever waiting for starter EOF; the close
	// simply finishes the one-request protocol. A close failure after a
	// successful send is a preserved diagnostic, never a decision: the
	// handshake still reads, decodes, and binds the one reply, and the
	// diagnostic joins every later outcome, accepted or not.
	if supervisorStartCloseRequestProbe != nil {
		supervisorStartCloseRequestProbe()
	}
	var closeRequestDiag error
	if closeReqErr := proc.CloseRequest(); closeReqErr != nil {
		closeRequestDiag = fmt.Errorf("start supervisor handoff: close request writer: %w", closeReqErr)
	}

	// Phase 5 — read exactly one reply frame. The receive runs on a
	// worker goroutine so a child that never replies cannot stall
	// cancellation.
	type receiveOutcome struct {
		frame []byte
		err   error
	}
	receiveDone := make(chan receiveOutcome, 1)
	go func() {
		frame, readErr := proc.Receive()
		receiveDone <- receiveOutcome{frame: frame, err: readErr}
	}()
	var frame []byte
	select {
	case outcome := <-receiveDone:
		if outcome.err != nil {
			closeErr := proc.Close()
			return supervisorStartHandoffResult{}, joinSupervisorStartErrors(
				fmt.Errorf("start supervisor handoff: read reply frame: %w", outcome.err), closeErr, closeRequestDiag)
		}
		frame = outcome.frame
	case <-ctx.Done():
		closeErr := proc.Close()
		<-receiveDone
		return supervisorStartHandoffResult{}, joinSupervisorStartErrors(
			fmt.Errorf("start supervisor handoff: %w", ctx.Err()), closeErr, closeRequestDiag)
	}

	// Phase 6 — the cancellation linearization point: one complete reply
	// frame is in hand but nothing has been decoded or bound. The probe
	// runs before the final cancellation check so a probe-fired
	// cancellation deterministically lands at this point, and a
	// cancellation observed here still closes and reaps the child.
	if supervisorStartCancelProbe != nil {
		supervisorStartCancelProbe(supervisorStartCancelBeforeDecode)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		closeErr := proc.Close()
		return supervisorStartHandoffResult{}, joinSupervisorStartErrors(
			fmt.Errorf("start supervisor handoff: %w", ctxErr), closeErr, closeRequestDiag)
	}

	// Phase 7 — strict decode. Once decoding begins, cancellation can no
	// longer act: acceptance wins from here on.
	reply, decodeErr := decodeSupervisorStartReply(frame)
	if decodeErr != nil {
		closeErr := proc.Close()
		return supervisorStartHandoffResult{}, joinSupervisorStartErrors(
			fmt.Errorf("start supervisor handoff: decode reply frame: %w", decodeErr), closeErr, closeRequestDiag)
	}

	// A complete rejection is a decided non-accepted answer: the child
	// already rolled back its own durable state before replying. Return
	// accepted=false with the bounded rejection reason, and close and reap
	// the child.
	if !reply.accepted() {
		rejectErr := fmt.Errorf("start supervisor handoff: supervisor rejected the start request: %s",
			boundSupervisorStartRejection(reply.reason))
		closeErr := proc.Close()
		return supervisorStartHandoffResult{}, joinSupervisorStartErrors(rejectErr, closeErr, closeRequestDiag)
	}

	// Phase 8 — bind the accepted reply to the request that was actually
	// sent (the re-decode of the payload is byte-identical to the request
	// the child decoded) and to the exact spawned supervisor PID.
	expected, rebindErr := decodeSupervisorStartRequest(payload)
	if rebindErr != nil {
		closeErr := proc.Close()
		return supervisorStartHandoffResult{}, joinSupervisorStartErrors(
			fmt.Errorf("start supervisor handoff: rebind request: %w", rebindErr), closeErr, closeRequestDiag)
	}
	if bindErr := bindSupervisorStartAccepted(expected, reply.snapshot, supervisorPID); bindErr != nil {
		closeErr := proc.Close()
		return supervisorStartHandoffResult{}, joinSupervisorStartErrors(
			fmt.Errorf("start supervisor handoff: bind accepted reply: %w", bindErr), closeErr, closeRequestDiag)
	}

	// Phase 9 — the reply is fully decoded and bound: acceptance is
	// committed. No worker is in flight, so Detach races nothing, and no
	// later cancellation is consulted: acceptance wins.
	result = supervisorStartHandoffResult{snapshot: reply.snapshot, accepted: true}
	if supervisorStartCancelProbe != nil {
		supervisorStartCancelProbe(supervisorStartCancelAfterBind)
	}
	// Acceptance is committed: the supervisor is detached exactly as
	// usual. Neither a close-request diagnostic nor a detach diagnostic
	// rolls the acceptance back, kills the supervisor, or triggers
	// Close; both join the accepted return.
	detachErr := proc.Detach()
	if closeRequestDiag != nil || detachErr != nil {
		var detachDiag error
		if detachErr != nil {
			detachDiag = fmt.Errorf("start supervisor handoff: detach accepted supervisor: %w", detachErr)
		}
		return result, joinSupervisorStartErrors(closeRequestDiag, detachDiag)
	}
	return result, nil
}

// bindSupervisorStartAccepted verifies that an accepted reply snapshot is
// bound to exactly the request the starter sent — run id, accepted time,
// the initial snapshot update time stamped from that same accepted
// instant, workspace, worktree, worker order and task projections, and
// execution timeout — and to the exact spawned supervisor PID. Every
// mismatch is reported; any mismatch is a protocol failure and the
// handoff must not accept the supervisor.
func bindSupervisorStartAccepted(req supervisorStartRequest, snap Snapshot, supervisorPID int) error {
	var errs []error

	if snap.RunID != req.runID {
		errs = append(errs, fmt.Errorf("runId %q does not match request runId %q", snap.RunID, req.runID))
	}

	// NewSnapshot normalizes the accepted time to UTC truncated to whole
	// seconds before building the snapshot and stamps the initial
	// snapshot's UpdatedAt with that same instant, so binding compares
	// the normalized whole-second UTC time on both stamps.
	wantAcceptedAt := req.acceptedAt.UTC().Truncate(time.Second)
	if !snap.AcceptedAt.Equal(wantAcceptedAt) {
		errs = append(errs, fmt.Errorf("acceptedAt %s does not match request acceptedAt %s",
			snap.AcceptedAt.Format(time.RFC3339), wantAcceptedAt.Format(time.RFC3339)))
	}
	if !snap.UpdatedAt.Equal(wantAcceptedAt) {
		errs = append(errs, fmt.Errorf("updatedAt %s does not match the initial accepted time %s",
			snap.UpdatedAt.Format(time.RFC3339), wantAcceptedAt.Format(time.RFC3339)))
	}

	if snap.Workspace != req.workspace {
		errs = append(errs, fmt.Errorf("workspace %q does not match request workspace %q", snap.Workspace, req.workspace))
	}

	if !equalWorktree(req.worktree, snap.Worktree) {
		errs = append(errs, errors.New("worktree does not match the request worktree"))
	}

	projected := run.ProjectTasks(req.tasks)
	if len(snap.Workers) != len(req.tasks) {
		errs = append(errs, fmt.Errorf("snapshot carries %d workers for %d tasks", len(snap.Workers), len(req.tasks)))
	} else {
		wantTimeout := req.executionTimeout.String()
		for i := range req.tasks {
			worker := snap.Workers[i]
			// The snapshot crossed the wire, whose omitempty shape cannot
			// express an empty non-nil Writes or Data slice, while
			// ProjectTasks builds an empty non-nil Data slice; normalize
			// both sides to nil so the comparison runs at the fidelity the
			// wire can express.
			if !reflect.DeepEqual(normalizeProjectionEmpties(worker.Task), normalizeProjectionEmpties(projected[i])) {
				errs = append(errs, fmt.Errorf("worker %d task projection does not match task %d", i+1, i+1))
			}
			if worker.ExecutionTimeout != wantTimeout {
				errs = append(errs, fmt.Errorf("worker %d executionTimeout %q does not match request executionTimeout %q",
					i+1, worker.ExecutionTimeout, wantTimeout))
			}
		}
	}

	if snap.Supervisor.PID != supervisorPID {
		errs = append(errs, fmt.Errorf("snapshot supervisor pid %d does not match the spawned supervisor pid %d",
			snap.Supervisor.PID, supervisorPID))
	}

	return errors.Join(errs...)
}

// normalizeProjectionEmpties returns a copy of the task projection with
// every empty Writes and Data slice set to nil, mirroring how the wire's
// omitempty shape decodes.
func normalizeProjectionEmpties(t run.TaskProjection) run.TaskProjection {
	if len(t.Writes) == 0 {
		t.Writes = nil
	}
	if len(t.Data) == 0 {
		t.Data = nil
	}
	return t
}

// equalWorktree reports whether two optional prepared worktrees are
// identical, including equal nil-ness.
func equalWorktree(a, b *worktree.Prepared) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// maxSupervisorStartRejectionReason caps the rejection reason text the
// starter embeds in its returned error. The child answers with exactly
// one bounded frame, but the rejection reason itself may describe
// arbitrary prepare failures, so the returned error stays bounded
// regardless.
const maxSupervisorStartRejectionReason = 4096

// boundSupervisorStartRejection truncates a rejection reason at
// maxSupervisorStartRejectionReason bytes on a UTF-8 character boundary.
// Reasons at or under the cap are returned verbatim.
func boundSupervisorStartRejection(reason string) string {
	if len(reason) <= maxSupervisorStartRejectionReason {
		return reason
	}
	cut := reason[:maxSupervisorStartRejectionReason]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

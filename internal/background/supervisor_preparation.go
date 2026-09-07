package background

import (
	"errors"
	"fmt"

	"github.com/arasovic/pi-worker/internal/admission"
)

// supervisorPreparation is the unconfirmed result of one production
// preparation transaction for a supervisor start: the accepted Snapshot
// already persisted by store, plus one admission QueueTicket per worker
// kept in task order, so tickets[i] belongs to worker ID i+1.
// snapshotCreated records whether this transaction's Store.Create
// succeeded; rollback consults it before removing the Snapshot, so a
// preparation whose create failed never removes a Snapshot that was
// never written. A preparation is normally held only between the
// durable Store.Create and the confirmation of an accepted reply:
// rollback undoes it and is retryable until it returns nil, and once
// the accepted reply is on the wire the tickets are owned by the run
// lifecycle. The exception is the retry-only preparation returned when
// Store.Create failed and cancelling the tickets also failed: it holds
// no persisted Snapshot, and exists only so the caller can retry
// rollback, which re-cancels the tickets still queued.
type supervisorPreparation struct {
	// snapshotCreated is set to true only after Store.Create succeeds
	// and cleared again only after rollback successfully removes the
	// Snapshot.
	snapshotCreated bool
	snapshot        Snapshot
	store           *Store
	tickets         []*admission.QueueTicket
}

// prepareSupervisorStart runs the whole production preparation
// transaction for one accepted supervisor start request:
//
//  1. validate the request;
//  2. construct the snapshot Store under req.backgroundRoot;
//  3. open the admission Gate over req.admissionRoot with
//     req.maxModelWorkers live slots;
//  4. read the Gate.OwnerIdentity — admission.Open already guarantees
//     both fields positive, and OwnerIdentity is the stored projection
//     of the identity Open sampled, so no re-validation is needed;
//  5. build the accepted Snapshot with NewSnapshot using exactly that
//     identity — the only prompt persistence here is the snapshot
//     projection, raw task material never reaches disk;
//  6. build worker IDs 1..N and enqueue the whole batch in one atomic
//     Gate.Prepare call;
//  7. Store.Create the accepted Snapshot. Tickets are prepared first
//     and outlive nothing by themselves; on a Create failure every
//     ticket is cancelled before a normal nil-preparation return, and
//     the preparation is returned only when that cancellation itself
//     failed, so the caller can retry rollback.
//
// If Store.Create fails, every prepared ticket is cancelled — all cancels
// are attempted, never stopping at the first failure. When every cancel
// succeeds, nothing remains to clean up: the create error is returned
// alone with a nil preparation. When any cancel fails, the preparation is
// returned together with the joined create and cancel errors so the
// caller can retry rollback, which re-cancels the tickets still queued;
// the snapshotCreated flag stays false, so that rollback never attempts
// to remove a Snapshot that was never created. Every earlier failure
// returns before anything is persisted, so nothing needs cleanup. On
// success the returned preparation is the caller's to confirm as an
// accepted reply or to roll back.
func prepareSupervisorStart(req supervisorStartRequest) (*supervisorPreparation, error) {
	// Phase 1 — validate the request before touching any state.
	if err := validateSupervisorStartRequest(req); err != nil {
		return nil, fmt.Errorf("prepare supervisor start (run %s): validate request: %w", req.runID, err)
	}

	// Phase 2 — construct the snapshot store.
	store, err := NewStore(req.backgroundRoot)
	if err != nil {
		return nil, fmt.Errorf("prepare supervisor start (run %s): construct store: %w", req.runID, err)
	}

	// Phase 3 — open the admission gate.
	gate, err := admission.Open(req.admissionRoot, req.maxModelWorkers)
	if err != nil {
		return nil, fmt.Errorf("prepare supervisor start (run %s): open admission gate: %w", req.runID, err)
	}

	// Phase 4 — read the gate's stored owner identity. Open already
	// verified a positive identity before returning, and OwnerIdentity
	// is the stored projection of the identity Open sampled, so no
	// re-validation is needed here.
	owner := gate.OwnerIdentity()

	// Phase 5 — build the accepted snapshot using exactly that identity.
	snapshot, err := NewSnapshot(
		req.runID,
		req.acceptedAt,
		req.workspace,
		ProcessIdentity{PID: owner.PID, CreateTime: owner.CreateTime},
		req.tasks,
		req.executionTimeout,
		req.worktree,
	)
	if err != nil {
		return nil, fmt.Errorf("prepare supervisor start (run %s): build accepted snapshot: %w", req.runID, err)
	}

	// Phase 6 — build worker IDs 1..N and prepare the whole batch in one
	// atomic admission transition. Tickets come back in task order.
	requests := make([]admission.Request, len(req.tasks))
	for i := range req.tasks {
		requests[i] = admission.Request{RunID: req.runID, WorkerID: i + 1}
	}
	tickets, err := gate.Prepare(requests)
	if err != nil {
		return nil, fmt.Errorf("prepare supervisor start (run %s): prepare admission tickets: %w", req.runID, err)
	}

	// Phase 7 — persist the accepted snapshot after the tickets. If Create
	// fails, nothing was persisted, so cancel every ticket before a normal
	// nil-preparation return. A fully successful cancel round leaves
	// nothing to clean up: return nil with the create error alone. When
	// any cancel fails, return the preparation with the joined errors so
	// the caller can retry rollback; the snapshotCreated flag is still
	// false, so rollback only re-cancels the tickets that are still
	// queued.
	prep := &supervisorPreparation{snapshot: snapshot, store: store, tickets: tickets}
	if err := store.Create(snapshot); err != nil {
		createErr := fmt.Errorf("prepare supervisor start (run %s): create snapshot: %w", req.runID, err)
		if cancelErr := cancelPreparedTickets(req.runID, tickets); cancelErr != nil {
			return prep, errors.Join(createErr, cancelErr)
		}
		return nil, createErr
	}

	// The Snapshot is durable; record that this preparation created it.
	prep.snapshotCreated = true
	return prep, nil
}

// rollback undoes an unconfirmed preparation and is retryable: every
// step it cannot complete leaves the preparation in a state where a
// later rollback call finishes the job, and a rollback that returns nil
// has fully undone the preparation. It removes the persisted Snapshot
// only while snapshotCreated is true, clearing that flag only after a
// successful removal, so a failed removal is retried by the next call
// while an already-absent or never-created Snapshot is never removed
// again. It then cancels every prepared ticket, attempting all of them;
// after a fully successful cancel round the ticket slice is cleared,
// while any cancel failure retains it so the next rollback re-cancels
// the tickets still queued (QueueTicket.Cancel is idempotent, so
// re-cancelling already-cancelled tickets is safe). Repeated rollback
// after full success returns nil without touching anything. rollback is
// used only before an accepted reply is confirmed; once the reply is
// confirmed, the tickets are owned by the run lifecycle and must never
// be cancelled here.
func (p *supervisorPreparation) rollback() error {
	if p == nil {
		return nil
	}
	var errs []error

	// Remove the snapshot first, but only when this preparation created
	// it. A zero-value preparation and a preparation whose Store.Create
	// failed both carry snapshotCreated false; a fully rolled-back
	// preparation has already cleared the flag after its successful
	// removal. A removal failure keeps the flag set so the caller can
	// retry rollback once the cause is gone.
	if p.snapshotCreated {
		if err := p.store.Remove(p.snapshot.RunID); err != nil {
			errs = append(errs, fmt.Errorf("rollback supervisor preparation (run %s): remove accepted snapshot: %w", p.snapshot.RunID, err))
		} else {
			p.snapshotCreated = false
		}
	}

	// Cancel every ticket, attempting all of them. Clear the slice only
	// after every cancel succeeded; on any failure keep it so a retry
	// re-cancels the tickets that remain queued.
	if err := cancelPreparedTickets(p.snapshot.RunID, p.tickets); err != nil {
		errs = append(errs, err)
	} else {
		p.tickets = nil
	}

	return errors.Join(errs...)
}

// cancelPreparedTickets cancels every prepared ticket in order,
// attempting all of them even when earlier cancels fail, and joins the
// resulting errors. Tickets are kept in task order, so the ticket at
// index i covers worker ID i+1; a nil ticket is skipped. QueueTicket.Cancel
// is idempotent, so repeated rollback-style cleanup is safe.
func cancelPreparedTickets(runID string, tickets []*admission.QueueTicket) error {
	var errs []error
	for i, ticket := range tickets {
		if ticket == nil {
			continue
		}
		if err := ticket.Cancel(); err != nil {
			errs = append(errs, fmt.Errorf("cancel prepared ticket %d (run %s): %w", i+1, runID, err))
		}
	}
	return errors.Join(errs...)
}

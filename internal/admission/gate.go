package admission

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

// Request describes one admission request.
type Request struct {
	RunID    string
	WorkerID int
}

// Gate serializes admission of prompt requests against a shared state file.
type Gate struct {
	root    string
	maxLive int
	owner   ownerIdentity
}

// QueueTicket represents a durably enqueued ticket that has not yet been
// granted as a Lease. It provides access to the ticket ID for later polling.
type QueueTicket struct {
	mu        sync.Mutex
	gate      *Gate
	ticketID  string
	owner     ownerIdentity
	waited    bool
	cancelled bool
}

// Lease holds a granted admission ticket.
type Lease struct {
	mu       sync.Mutex // protects Lease release idempotency
	gate     *Gate
	ticketID string
	owner    ownerIdentity
	released bool
}

// pollInterval is the time between polls while waiting for a queued ticket
// to become grantable. It is a private test seam.
var pollInterval = 50 * time.Millisecond

// newTicketID generates a random 128-bit hex ticket ID. It is a private
// test seam for deterministic tests.
var newTicketID = defaultNewTicketID

func defaultNewTicketID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate ticket id: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// Open creates a Gate rooted at root with at most maxLive concurrent leases.
// It rejects empty root and non-positive maxLive. On supported platforms it
// obtains and releases the file lock once, loads strict state under the lock
// (so corrupt or symlink state fails closed), and verifies a positive current
// owner identity before returning. Other platforms return ErrUnsupported.
func Open(root string, maxLive int) (*Gate, error) {
	if root == "" {
		return nil, errors.New("admission: root must not be empty")
	}
	if maxLive <= 0 {
		return nil, fmt.Errorf("admission: maxLive must be positive, got %d", maxLive)
	}

	unlock, err := lockState(root)
	if err != nil {
		if errors.Is(err, ErrUnsupported) {
			return nil, err
		}
		return nil, fmt.Errorf("admission open: %w", err)
	}
	defer unlock()

	if _, err := loadState(root); err != nil {
		return nil, fmt.Errorf("admission open: %w", err)
	}

	owner, err := currentOwner()
	if err != nil {
		return nil, fmt.Errorf("admission open: %w", err)
	}
	if owner.PID <= 0 || owner.CreateTime <= 0 {
		return nil, fmt.Errorf("admission open: owner identity is not positive")
	}

	return &Gate{root: root, maxLive: maxLive, owner: owner}, nil
}

// updateState performs a locked state transition. It acquires the lock,
// loads the state, reaps stale tickets, applies update under the lock, and
// saves when either the reaping or update changed the state. Lock, load,
// and save errors are wrapped with "lock:", "load:", and "save:" so callers
// can add their own context. If update returns an action error while the
// reaping changed the state, the reaping is persisted first; a failed save
// then takes precedence because the transition was not durable. No save
// happens when neither the reaping nor update changed the state.
func (g *Gate) updateState(update func(*state) (changed bool, err error)) error {
	unlock, err := lockState(g.root)
	if err != nil {
		return fmt.Errorf("lock: %w", err)
	}
	defer unlock()

	st, err := loadState(g.root)
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}

	retained, reaped := reapStale(st.Tickets)
	st.Tickets = retained

	changed, actionErr := update(&st)

	if reaped || changed {
		if saveErr := saveState(g.root, st); saveErr != nil {
			return fmt.Errorf("save: %w", saveErr)
		}
	}

	return actionErr
}

// Enqueue writes exactly one durable queued ticket and never waits for
// capacity. It validates non-empty RunID and positive WorkerID, generates
// one random ticket ID, and persists a single queued ticket under one lock
// transition. The returned QueueTicket can later be used to poll for grant.
func (g *Gate) Enqueue(req Request) (*QueueTicket, error) {
	if req.RunID == "" {
		return nil, errors.New("admission enqueue: runId must not be empty")
	}
	if req.WorkerID <= 0 {
		return nil, fmt.Errorf("admission enqueue: workerId must be positive, got %d", req.WorkerID)
	}

	ticketID, err := newTicketID()
	if err != nil {
		return nil, fmt.Errorf("admission enqueue: %w", err)
	}

	// Enqueue the ticket under one lock transition.
	if err := g.updateState(func(st *state) (bool, error) {
		if st.NextSequence > math.MaxInt-1 {
			return false, errors.New("sequence overflow")
		}
		seq := st.NextSequence
		st.NextSequence++
		st.Tickets = append(st.Tickets, ticket{
			ID:              ticketID,
			Sequence:        seq,
			RunID:           req.RunID,
			WorkerID:        req.WorkerID,
			OwnerPID:        g.owner.PID,
			OwnerCreateTime: g.owner.CreateTime,
			State:           ticketQueued,
		})
		return true, nil
	}); err != nil {
		return nil, fmt.Errorf("admission enqueue: %w", err)
	}

	return &QueueTicket{gate: g, ticketID: ticketID, owner: g.owner}, nil
}

// tryGrant reacquires the lock, reaps stale tickets, checks whether ticketID
// is the earliest queued ticket and leased count is below maxLive. If so it
// marks the ticket leased and returns a Lease. Returns (nil, nil) if the
// ticket is not yet grantable. Returns an error if the ticket was reaped.
func (g *Gate) tryGrant(ctx context.Context, ticketID string, owner ownerIdentity) (*Lease, error) {
	var lease *Lease
	err := g.updateState(func(st *state) (bool, error) {
		// Find our ticket.
		idx := findTicketIndex(st.Tickets, ticketID)
		if idx < 0 {
			return false, fmt.Errorf("ticket %q not found: reaped while waiting", ticketID)
		}
		t := &st.Tickets[idx]

		// Only lease when the ticket is still queued and is the earliest queued ticket.
		if t.State != ticketQueued {
			return false, nil
		}

		// Verify it is the earliest queued ticket.
		for i := range st.Tickets {
			other := &st.Tickets[i]
			if other.State == ticketQueued && other.Sequence < t.Sequence {
				return false, nil
			}
		}

		// Count current leased tickets.
		leased := 0
		for i := range st.Tickets {
			if st.Tickets[i].State == ticketLeased {
				leased++
			}
		}
		if leased >= g.maxLive {
			return false, nil
		}

		// Check context before committing to lease.
		if err := ctx.Err(); err != nil {
			return false, err
		}

		// Grant the lease.
		t.State = ticketLeased
		lease = &Lease{
			gate:     g,
			ticketID: ticketID,
			owner:    owner,
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return lease, nil
}

// removeQueuedTicket reacquires the lock and removes only the exact queued
// ticket identified by ticketID. Leased tickets are never removed. Stale
// tickets are reaped. The state is saved only if any ticket was reaped or
// removed. Lock, load, and save errors are wrapped with "admission cleanup:"
// context.
func (g *Gate) removeQueuedTicket(ticketID string) error {
	err := g.updateState(func(st *state) (bool, error) {
		removed := false
		for i := range st.Tickets {
			if st.Tickets[i].ID == ticketID {
				if st.Tickets[i].State == ticketQueued {
					st.Tickets = append(st.Tickets[:i], st.Tickets[i+1:]...)
					removed = true
				}
				break
			}
		}
		return removed, nil
	})
	if err != nil {
		return fmt.Errorf("admission cleanup: %w", err)
	}
	return nil
}

// Wait blocks until the queued ticket is granted as a Lease, the context
// is cancelled, or the ticket is reaped. Wait must be called at most once
// per QueueTicket; a second call returns a deterministic error before
// touching durable state. It never calls Enqueue or changes NextSequence.
//
// If the context is already done or ends while queued, Wait cancels the
// ticket and returns a joined error. Any tryGrant error cancels the ticket
// and returns a joined error. After a Lease is granted, the context is
// checked as the return linearization point; if done, the lease is released
// and a joined error is returned.
func (t *QueueTicket) Wait(ctx context.Context) (*Lease, error) {
	t.mu.Lock()
	if t.waited {
		t.mu.Unlock()
		return nil, errors.New("admission wait: already waited")
	}
	if t.cancelled {
		t.mu.Unlock()
		return nil, errors.New("admission wait: ticket cancelled")
	}
	t.waited = true
	t.mu.Unlock()

	// Reject if context is already done before any file/poll wait.
	if err := ctx.Err(); err != nil {
		cancelErr := t.Cancel()
		return nil, errors.Join(fmt.Errorf("admission wait: %w", err), cancelErr)
	}

	// Poll for grant, exactly as old Acquire did.
	for {
		lease, err := t.gate.tryGrant(ctx, t.ticketID, t.owner)
		if err != nil {
			cancelErr := t.Cancel()
			return nil, errors.Join(fmt.Errorf("admission wait: %w", err), cancelErr)
		}
		if lease != nil {
			// Check context as the return linearization point.
			if err := ctx.Err(); err != nil {
				releaseErr := lease.Release()
				cancelErr := t.Cancel()
				return nil, errors.Join(err, releaseErr, cancelErr)
			}
			return lease, nil
		}
		// Wait for the next poll interval.
		select {
		case <-ctx.Done():
			cancelErr := t.Cancel()
			return nil, errors.Join(ctx.Err(), cancelErr)
		case <-time.After(pollInterval):
		}
	}
}

// Cancel removes the queued ticket from the gate. It can be called before
// Wait for batch rollback or while Wait is blocked. Cancel never removes
// a lease. On success Cancel is idempotent; on error the caller may retry
// because the ticket remains in its previous state.
func (t *QueueTicket) Cancel() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.cancelled {
		return nil
	}

	err := t.gate.removeQueuedTicket(t.ticketID)
	if err != nil {
		return err
	}

	t.cancelled = true
	return nil
}

// Release removes the leased ticket under the lock and returns. Release is
// idempotent; repeated calls return nil without another state change.
func (l *Lease) Release() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.released {
		return nil
	}

	if err := l.releaseUnderLock(); err != nil {
		return err
	}
	l.released = true
	return nil
}

// releaseUnderLock does the actual release. Caller must hold l.mu.
func (l *Lease) releaseUnderLock() error {
	err := l.gate.updateState(func(st *state) (bool, error) {
		idx := findTicketIndex(st.Tickets, l.ticketID)
		if idx < 0 {
			return false, fmt.Errorf("ticket %q not found", l.ticketID)
		}

		t := &st.Tickets[idx]
		if t.State != ticketLeased {
			return false, fmt.Errorf("ticket %q is not leased (state=%s)", l.ticketID, t.State)
		}
		if t.OwnerPID != l.owner.PID || t.OwnerCreateTime != l.owner.CreateTime {
			return false, fmt.Errorf("ticket %q owner mismatch", l.ticketID)
		}

		st.Tickets = append(st.Tickets[:idx], st.Tickets[idx+1:]...)
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("admission release: %w", err)
	}
	return nil
}

// reapStale removes tickets whose owner status is ownerStale. ownerSame and
// ownerUncertain tickets are retained. It reports whether any ticket was
// removed so callers know when a save is required to persist the reaping.
// This must be called under the lock.
func reapStale(tickets []ticket) (retained []ticket, changed bool) {
	n := 0
	for _, t := range tickets {
		status := ownerStatus(ownerIdentity{PID: t.OwnerPID, CreateTime: t.OwnerCreateTime})
		if status != ownerStale {
			tickets[n] = t
			n++
		}
	}
	return tickets[:n], n != len(tickets)
}

// findTicketIndex returns the index of the ticket with the given ID, or -1.
func findTicketIndex(tickets []ticket, id string) int {
	for i := range tickets {
		if tickets[i].ID == id {
			return i
		}
	}
	return -1
}

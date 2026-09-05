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

// Acquire enqueues a ticket for req and blocks until the ticket is granted
// as a Lease or the context is cancelled. It validates non-empty RunID and
// positive WorkerID, generates one random ticket ID, and enqueues exactly
// one queued ticket under a single lock transition. While waiting outside
// the lock, it polls with a bounded interval respecting caller context.
// If context ends while the ticket is still queued, Acquire reacquires the
// lock, removes only that exact queued ticket, and returns an error matching
// ctx.Err().
func (g *Gate) Acquire(ctx context.Context, req Request) (*Lease, error) {
	if req.RunID == "" {
		return nil, errors.New("admission acquire: runId must not be empty")
	}
	if req.WorkerID <= 0 {
		return nil, fmt.Errorf("admission acquire: workerId must be positive, got %d", req.WorkerID)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	ticketID, err := newTicketID()
	if err != nil {
		return nil, fmt.Errorf("admission acquire: %w", err)
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
		return nil, fmt.Errorf("admission acquire: %w", err)
	}

	// Poll outside the lock until our ticket is grantable or context ends.
	for {
		if ctx.Err() != nil {
			return nil, g.removeTicketAndReturnCtxErr(ctx, ticketID)
		}

		grantable, err := g.tryGrant(ctx, ticketID, g.owner)
		if err != nil {
			cleanupErr := g.removeQueuedTicket(ticketID)
			return nil, errors.Join(fmt.Errorf("admission acquire: %w", err), cleanupErr)
		}
		if grantable != nil {
			// Return linearization point: check context once more.
			if ctx.Err() != nil {
				releaseErr := grantable.Release()
				return nil, errors.Join(ctx.Err(), releaseErr)
			}
			return grantable, nil
		}

		select {
		case <-ctx.Done():
			return nil, g.removeTicketAndReturnCtxErr(ctx, ticketID)
		case <-time.After(pollInterval):
		}
	}
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

// removeTicketAndReturnCtxErr reacquires the lock and removes only the exact
// queued ticket identified by ticketID before returning an error that matches
// ctx.Err(). If cleanup also fails, both errors are preserved. Leased tickets
// are never removed on this path.
func (g *Gate) removeTicketAndReturnCtxErr(ctx context.Context, ticketID string) error {
	cleanupErr := g.removeQueuedTicket(ticketID)
	if cleanupErr != nil {
		return errors.Join(ctx.Err(), cleanupErr)
	}
	return ctx.Err()
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

// Package admission provides durable state for the admission controller:
// a schema-versioned document of ordered tickets that gates and schedules
// prompt execution on worker processes.
package admission

import (
	"fmt"
)

// schemaVersion is the only supported document version.
const schemaVersion = 1

// Ticket states.
const (
	ticketQueued = "queued"
	ticketLeased = "leased"
)

// state is the durable admission document stored as state.json.
// An empty state is valid: schemaVersion 1, nextSequence 1, no tickets.
type state struct {
	SchemaVersion int      `json:"schemaVersion"`
	NextSequence  int      `json:"nextSequence"`
	Tickets       []ticket `json:"tickets"`
}

// ticket represents one admission request waiting for or holding a lease.
type ticket struct {
	ID              string `json:"id"`
	Sequence        int    `json:"sequence"`
	RunID           string `json:"runId"`
	WorkerID        int    `json:"workerId"`
	OwnerPID        int    `json:"ownerPid"`
	OwnerCreateTime int64  `json:"ownerCreateTime"`
	State           string `json:"state"`
}

// emptyState returns the valid empty admission state: schema version 1,
// starting nextSequence so the first issued sequence is positive, and no
// tickets.
func emptyState() state {
	return state{
		SchemaVersion: 1,
		NextSequence:  1,
		Tickets:       []ticket{},
	}
}

// validateState reports whether s is a well-formed admission state.
// It rejects unsupported schema versions, non-positive nextSequence,
// tickets not ordered by ascending sequence, duplicate ticket IDs or
// sequences, nextSequence not strictly greater than every existing
// sequence, and any ticket with zero or missing fields or an unknown
// state string.
func validateState(s state) error {
	if s.SchemaVersion != schemaVersion {
		return fmt.Errorf("unsupported schemaVersion %d: want %d", s.SchemaVersion, schemaVersion)
	}
	if s.NextSequence <= 0 {
		return fmt.Errorf("nextSequence must be positive, got %d", s.NextSequence)
	}
	if s.Tickets == nil {
		s.Tickets = []ticket{}
	}
	seenIDs := make(map[string]bool, len(s.Tickets))
	var lastSeq int
	for i, t := range s.Tickets {
		if t.ID == "" {
			return fmt.Errorf("ticket %d: id must be non-empty", i)
		}
		if seenIDs[t.ID] {
			return fmt.Errorf("duplicate ticket id %q", t.ID)
		}
		seenIDs[t.ID] = true

		if t.Sequence <= 0 {
			return fmt.Errorf("ticket %d (%s): sequence must be positive, got %d", i, t.ID, t.Sequence)
		}
		if i > 0 && t.Sequence <= lastSeq {
			return fmt.Errorf("ticket %d (%s): sequence %d must be greater than previous %d", i, t.ID, t.Sequence, lastSeq)
		}
		lastSeq = t.Sequence

		if t.RunID == "" {
			return fmt.Errorf("ticket %d (%s): runId must be non-empty", i, t.ID)
		}
		if t.WorkerID <= 0 {
			return fmt.Errorf("ticket %d (%s): workerId must be positive, got %d", i, t.ID, t.WorkerID)
		}
		if t.OwnerPID <= 0 {
			return fmt.Errorf("ticket %d (%s): ownerPid must be positive, got %d", i, t.ID, t.OwnerPID)
		}
		if t.OwnerCreateTime <= 0 {
			return fmt.Errorf("ticket %d (%s): ownerCreateTime must be positive, got %d", i, t.ID, t.OwnerCreateTime)
		}
		if t.State != ticketQueued && t.State != ticketLeased {
			return fmt.Errorf("ticket %d (%s): unknown state %q: want %q or %q", i, t.ID, t.State, ticketQueued, ticketLeased)
		}
	}
	if lastSeq >= s.NextSequence {
		return fmt.Errorf("nextSequence %d must be greater than the largest existing sequence %d", s.NextSequence, lastSeq)
	}
	return nil
}

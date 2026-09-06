//go:build darwin || linux

package admission

import (
	"fmt"
	"sync"
	"testing"
)

// TestPrepareBatchValidationErrors checks every input-validation path
// inside Prepare: mismatched RunID, duplicate WorkerID, empty RunID,
// zero WorkerID, negative WorkerID. Each case must return nil handles,
// an error prefixed with "admission prepare:", and leave durable state
// completely unchanged — including not consuming any ticket ID.
func TestPrepareBatchValidationErrors(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 5)
	stateBefore := readStateForTest(t, root)
	nIDs := installSequentialTicketIDs(t, "VB-")
	sc := installSaveCounter(t)

	tests := []struct {
		label string
		req   []Request
	}{
		{"mixed RunID", []Request{{RunID: "a", WorkerID: 1}, {RunID: "b", WorkerID: 2}}},
		{"duplicate WorkerID", []Request{{RunID: "a", WorkerID: 1}, {RunID: "a", WorkerID: 1}}},
		{"empty RunID", []Request{{RunID: "", WorkerID: 1}}},
		{"zero WorkerID", []Request{{RunID: "a", WorkerID: 0}}},
		{"negative WorkerID", []Request{{RunID: "a", WorkerID: -3}}},
	}
	for _, tt := range tests {
		handles, err := g.Prepare(tt.req)
		if err == nil {
			t.Errorf("Prepare(%s): got nil, want error", tt.label)
			continue
		}
		if prefix := "admission prepare:"; len(err.Error()) < len(prefix) || err.Error()[:len(prefix)] != prefix {
			t.Errorf("Prepare(%s): error=%q missing '%s' prefix", tt.label, err, prefix)
		}
		if handles != nil {
			t.Errorf("Prepare(%s): handles=%v, want nil", tt.label, handles)
		}
		// Validation rejects before generating any ticket IDs.
		if c := nIDs(); c != 0 {
			t.Errorf("Prepare(%s): nIDs=%d, want 0", tt.label, c)
		}
		if c := sc(); c != 0 {
			t.Errorf("Prepare(%s): saveGateState calls = %d, want 0", tt.label, c)
		}
		// State must be exactly unchanged.
		assertNoChange(t, readStateForTest(t, root), stateBefore)
	}
}

// TestPrepareNewTicketIDFailure verifies that when the ticket-ID
// generator fails part-way through a batch, Prepare returns nil
// handles, does not mutate durable state, and reports the failing
// request number. Only the IDs consumed before the failure point are
// "used"; the counter-based seam prevents reuse collisions anyway.
func TestPrepareNewTicketIDFailure(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 5)
	stateBefore := readStateForTest(t, root)
	sc := installSaveCounter(t)

	batch := []Request{
		{RunID: "r", WorkerID: 1},
		{RunID: "r", WorkerID: 2},
		{RunID: "r", WorkerID: 3},
	}
	failAt := []int{1, 2, 3} // 1-based position that should fail
	prevSeed := "FA-"
	var mu sync.Mutex
	totalCalls := 0

	for _, pos := range failAt {
		nCalled := 0
		newTicketID = func() (string, error) {
			mu.Lock()
			defer mu.Unlock()
			nCalled++
			if nCalled == pos {
				return "", fmt.Errorf("boom %d", nCalled)
			}
			totalCalls++
			return prevSeed + padInt(totalCalls, 6), nil
		}

		handles, err := g.Prepare(batch)
		if handles != nil {
			t.Errorf("failAt=%d: handles=%v, want nil", pos, handles)
		}
		if err == nil {
			t.Errorf("failAt=%d: got nil, want error", pos)
			continue
		}
		prefix := "admission prepare:"
		if len(fmt.Sprintf("%v", err)) < len(prefix) || fmt.Sprintf("%v", err)[:len(prefix)] != prefix {
			t.Errorf("failAt=%d: error=%q missing '%s' prefix", pos, err, prefix)
		}
		if c := sc(); c != 0 {
			t.Errorf("failAt=%d: saveGateState calls = %d, want 0", pos, c)
		}
		st := readStateForTest(t, root)
		assertNoChange(t, st, stateBefore)
	}

	newTicketID = defaultNewTicketID
}

// TestPrepareDuplicateGeneratedIDs verifies that when the ticket-ID
// generator accidentally produces two identical IDs within one
// batch, Prepare returns nil handles, reports a prefixed error
// pointing at the second occurrence, and leaves state unchanged.
func TestPrepareDuplicateGeneratedIDs(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 5)
	stateBefore := readStateForTest(t, root)
	const dupID = "same-id"
	sc := installSaveCounter(t)

	prev := newTicketID
	var mu sync.Mutex
	newTicketID = func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		// Always return the same ID to force collision on every call.
		return dupID, nil
	}

	handles, err := g.Prepare([]Request{
		{RunID: "dup", WorkerID: 1},
		{RunID: "dup", WorkerID: 2},
	})
	if handles != nil {
		t.Errorf("handles=%v, want nil", handles)
	}
	if c := sc(); c != 0 {
		t.Fatalf("saveGateState calls = %d, want 0", c)
	}
	if err == nil {
		t.Fatal("got nil, want error")
	}
	prefix := "admission prepare:"
	if len(fmt.Sprintf("%v", err)) < len(prefix) || fmt.Sprintf("%v", err)[:len(prefix)] != prefix {
		t.Errorf("error=%q missing '%s' prefix", err, prefix)
	}
	assertNoChange(t, readStateForTest(t, root), stateBefore)

	if c := sc(); c != 0 {
		t.Errorf("post-error: saveGateState calls = %d, want 0", c)
	}

	newTicketID = prev
}

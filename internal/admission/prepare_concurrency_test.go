//go:build darwin || linux

package admission

import (
	"context"
	"sync"
	"testing"
)

// TestGateBoundedRacePrepareAndEnqueue exercises two Gate instances opened
// on the same root with maxLive=1 and deterministic thread-safe sequential
// ticket IDs. One goroutine calls Prepare for workers 1,2 of run "batch";
// the other calls Enqueue for run "single" worker 1. Results are collected
//
//	– durable state contains exactly three queued tickets (sequences 1,2,3);
//	– batch ticket IDs are contiguous in state (positions 0+1 or 1+2);
//	– no ticket is leased before a grant attempt;
//	– tryGrant on a non-earliest ticket returns nil lease;
//	– granting and releasing every ticket in sequence order leaves clean state.
func TestPrepareConcurrentWithEnqueuePreservesFIFO(t *testing.T) {
	root := t.TempDir()

	// Install a deterministic ticket-ID generator shared by both gates.
	consume := installSequentialTicketIDs(t, "bnd")
	restoreGateSeams(t, 1001, 1001000, map[int]pidResp{
		1001: {exists: true, createTime: 1001000},
	})

	gateA, err := Open(root, 1)
	if err != nil {
		t.Fatalf("Open gateA: %v", err)
	}
	gateB, err := Open(root, 1)
	if err != nil {
		t.Fatalf("Open gateB: %v", err)
	}

	// ── release two goroutines from one start channel ────────────────

	type opResult struct {
		tickets []*QueueTicket
		err     error
	}
	results := make(chan opResult, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})

	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		tickets, err := gateA.Prepare([]Request{
			{RunID: "batch", WorkerID: 1},
			{RunID: "batch", WorkerID: 2},
		})
		results <- opResult{tickets, err}
	}()
	go func() {
		defer wg.Done()
		<-start
		ticket, err := gateB.Enqueue(Request{RunID: "single", WorkerID: 1})
		res := opResult{nil, err}
		if ticket != nil {
			res.tickets = []*QueueTicket{ticket}
		}
		results <- res
	}()

	close(start)   // release both goroutines simultaneously
	wg.Wait()      // wait for completion, no sleeps
	close(results) // drain

	// Collect results from the buffered channel.
	var ra, rb opResult
	for r := range results {
		if r.err != nil {
			t.Fatalf("operation failed: %v", r.err)
		}
		if len(r.tickets) == 2 {
			ra = r
		} else {
			rb = r
		}
	}

	// ── verify durable state: three queued tickets, seqs 1,2,3 ──────

	st := readStateForTest(t, root)
	assertTicketCount(t, st, 3)
	assertNextSequence(t, st, 4)

	for i, tk := range st.Tickets {
		if tk.State != ticketQueued {
			t.Fatalf("ticket[%d] state=%q, want %s", i, tk.State, ticketQueued)
		}
	}

	// Locate the batch tickets' positions; they must be contiguous.
	batchPositions := []int{}
	for i, tk := range st.Tickets {
		if tk.RunID == "batch" {
			batchPositions = append(batchPositions, i)
		}
	}
	if len(batchPositions) != 2 {
		t.Fatalf("expected 2 batch tickets in state, got %d", len(batchPositions))
	}
	if batchPositions[1]-batchPositions[0] != 1 {
		t.Fatalf("batch tickets must be contiguous; positions %v", batchPositions)
	}

	// ── build handle map from returned QueueTickets by ticket ID ────

	allHandles := make([]*QueueTicket, 0, 3)
	allHandles = append(allHandles, ra.tickets...)
	allHandles = append(allHandles, rb.tickets...)

	handleByID := make(map[string]*QueueTicket, 3)
	for _, h := range allHandles {
		if _, dup := handleByID[h.ticketID]; dup {
			t.Fatalf("duplicate handle for ticket %q", h.ticketID)
		}
		handleByID[h.ticketID] = h
	}
	if got := len(handleByID); got != 3 {
		t.Fatalf("handleByID size = %d, want 3", got)
	}

	// Confirm every state ticket has a matching handle.
	for _, tk := range st.Tickets {
		if _, ok := handleByID[tk.ID]; !ok {
			t.Fatalf("no handle for state ticket %q", tk.ID)
		}
	}

	// ── tryGrant on a non-earliest ticket → nil lease (FIFO proof) ──

	ctx := context.Background()
	nonEarliest := st.Tickets[len(st.Tickets)-1].ID
	lease, err := gateA.tryGrant(ctx, nonEarliest, gateA.owner)
	if err != nil {
		t.Fatalf("tryGrant(non-earliest %q): %v", nonEarliest, err)
	}
	if lease != nil {
		t.Fatal("tryGrant(non-earliest) returned non-nil lease, want nil")
	}

	// State must still have three queued tickets (no accidental grant).
	st = readStateForTest(t, root)
	assertTicketCount(t, st, 3)

	// ── grant every ticket in durable sequence order, releasing each before the next ──

	for _, tk := range st.Tickets {
		l, err := gateA.tryGrant(ctx, tk.ID, gateA.owner)
		if err != nil {
			t.Fatalf("tryGrant(seq %d, %q): %v", tk.Sequence, tk.ID, err)
		}
		if l == nil {
			t.Fatalf("tryGrant(seq %d, %q) returned nil lease", tk.Sequence, tk.ID)
		}
		if err := l.Release(); err != nil {
			t.Fatalf("Release(seq %d): %v", tk.Sequence, err)
		}
	}

	// Final state: empty, NextSequence preserved.
	st = readStateForTest(t, root)
	assertNoTickets(t, root)
	assertNextSequence(t, st, 4)

	// Exhaust the deterministic seed counter for clarity.
	if n := consume(); n < 3 {
		t.Fatalf("ticket IDs consumed = %d, want >= 3", n)
	}
}

//go:build darwin || linux

package admission

import (
	"context"
	"sync"
	"testing"
)

// installSaveCounter installs a saveGateState wrapper that delegates to the
// original while recording every invocation. Returns a count accessor;
// the original is restored via t.Cleanup.
func installSaveCounter(t *testing.T) func() int {
	t.Helper()
	saved := saveGateState
	var mu sync.Mutex
	var n int
	saveGateState = func(p string, s state) error {
		mu.Lock()
		defer mu.Unlock()
		n++
		return saved(p, s)
	}
	t.Cleanup(func() { saveGateState = saved })
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// TestPrepareBatchThree verifies three requests are atomically enqueued with
// consecutive sequences under one durable state write.
func TestPrepareBatchThree(t *testing.T) {
	root := t.TempDir()
	g, owner := openGateForTest(t, root, 5)
	nIDs := installSequentialTicketIDs(t, "PT-")
	sc := installSaveCounter(t)

	handles, err := g.Prepare([]Request{
		{RunID: "run-1", WorkerID: 1},
		{RunID: "run-1", WorkerID: 2},
		{RunID: "run-1", WorkerID: 3},
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	for i, h := range handles {
		if h == nil {
			t.Errorf("handle[%d] is nil", i)
		}
	}
	if got := nIDs(); got != 3 {
		t.Fatalf("ticket IDs consumed = %d, want 3", got)
	}

	st := readStateForTest(t, root)
	assertTicketCount(t, st, 3)
	assertNextSequence(t, st, 4)

	wantWorkers := []int{1, 2, 3}
	wantSeqs := []int{1, 2, 3}
	for i, tk := range st.Tickets {
		switch {
		case tk.State != ticketQueued:
			t.Errorf("ticket[%d] state=%q want %q", i, tk.State, ticketQueued)
		case tk.RunID != "run-1":
			t.Errorf("ticket[%d] RunID=%q want run-1", i, tk.RunID)
		case tk.WorkerID != wantWorkers[i]:
			t.Errorf("ticket[%d] WorkerID=%d want %d", i, tk.WorkerID, wantWorkers[i])
		case tk.Sequence != wantSeqs[i]:
			t.Errorf("ticket[%d] Seq=%d want %d", i, tk.Sequence, wantSeqs[i])
		case tk.OwnerPID != owner.PID || tk.OwnerCreateTime != owner.CreateTime:
			t.Errorf("ticket[%d] owner mismatch", i)
		case tk.ID != handles[i].ticketID:
			t.Errorf("ticket[%d] ID != handle.ticketID", i)
		}
	}

	if c := sc(); c != 1 {
		t.Fatalf("saveGateState calls = %d, want 1", c)
	}

	// After Prepare each Wait grants the earliest queued in turn.
	for _, h := range handles {
		l, err := h.Wait(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if l == nil {
			t.Fatal("nil lease")
		}
		_ = l.Release()
	}
}

// TestPrepareCancelWhileQueued: maxLive=1, first Wait granted, second
// Cancel while queued, Release first → empty.
func TestPrepareCancelWhileQueued(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 1)
	nIDs := installSequentialTicketIDs(t, "PC-")

	handles, err := g.Prepare([]Request{{RunID: "sr", WorkerID: 10}, {RunID: "sr", WorkerID: 20}})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	ctx := context.Background()
	lease, err := handles[0].Wait(ctx)
	if err != nil || lease == nil {
		t.Fatalf("Wait first: err=%v lease=%p", err, lease)
	}
	st := readStateForTest(t, root)
	assertTicketCount(t, st, 2)
	if st.Tickets[0].State != ticketLeased {
		t.Fatalf("first state=%q want leased", st.Tickets[0].State)
	}
	cancelID := handles[1].ticketID
	if err := handles[1].Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	st = readStateForTest(t, root)
	assertTicketCount(t, st, 1)
	if st.Tickets[0].ID == cancelID {
		t.Fatalf("cancelled ticket %q still present", cancelID)
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	assertNoTickets(t, root)
	if got := nIDs(); got != 2 {
		t.Fatalf("IDs consumed = %d, want 2", got)
	}
}

// TestPrepareVersusEnqueue compares Prepare vs Enqueue lifecycle using
// separate roots; no random-ID matching needed.
func TestPrepareVersusEnqueue(t *testing.T) {
	ctx := context.Background()

	rootA := t.TempDir()
	gA, ownerA := openGateForTest(t, rootA, 1)
	installSequentialTicketIDs(t, "PA-")

	handlesA, err := gA.Prepare([]Request{{RunID: "a-run", WorkerID: 1}})
	if err != nil {
		t.Fatalf("Prepare A: %v", err)
	}
	leaseA, werrA := handlesA[0].Wait(ctx)
	if werrA != nil || leaseA == nil {
		t.Fatalf("Wait A: err=%v lease=%p", werrA, leaseA)
	}
	_ = leaseA.Release()
	stA := readStateForTest(t, rootA)
	assertNoTickets(t, rootA)
	assertNextSequence(t, stA, 2)

	rootB := t.TempDir()
	gB, _ := openGateForTest(t, rootB, 1)
	installSequentialTicketIDs(t, "PB-")

	qtB, err := gB.Enqueue(Request{RunID: "b-run", WorkerID: 1})
	if err != nil {
		t.Fatalf("Enqueue B: %v", err)
	}
	leaseB, werrB := qtB.Wait(ctx)
	if werrB != nil || leaseB == nil {
		t.Fatalf("Wait B: err=%v lease=%p", werrB, leaseB)
	}
	_ = leaseB.Release()
	stB := readStateForTest(t, rootB)
	assertNoTickets(t, rootB)
	assertNextSequence(t, stB, 2)

	if ownerA.PID <= 0 {
		t.Fatalf("Owner PID not positive: %d", ownerA.PID)
	}
}

// TestPrepareNilGateAndEmptyBatch edge cases without mutating state.
func TestPrepareNilGateAndEmptyBatch(t *testing.T) {
	root := t.TempDir()
	openGateForTest(t, root, 1)
	stateBefore := readStateForTest(t, root)

	var nilG *Gate
	if _, err := nilG.Prepare([]Request{{RunID: "r", WorkerID: 1}}); err == nil {
		t.Fatal("nil Prepare: got nil, want error")
	}
	assertNoChange(t, readStateForTest(t, root), stateBefore)

	if _, err := (&Gate{}).Prepare([]Request{}); err == nil {
		t.Fatal("empty Prepare: got nil, want error")
	}
	assertNoChange(t, readStateForTest(t, root), stateBefore)
}

// assertNoChange verifies st equals expected at all fields.
func assertNoChange(t *testing.T, st, expected state) {
	t.Helper()
	if st.SchemaVersion != expected.SchemaVersion {
		t.Fatalf("SchemaVersion changed: %d -> %d", expected.SchemaVersion, st.SchemaVersion)
	}
	if st.NextSequence != expected.NextSequence {
		t.Fatalf("NextSequence changed: %d -> %d", expected.NextSequence, st.NextSequence)
	}
	if len(st.Tickets) != len(expected.Tickets) {
		t.Fatalf("tickets count changed: %d -> %d", len(expected.Tickets), len(st.Tickets))
	}
	for i := range st.Tickets {
		exp := expected.Tickets[i]
		cur := st.Tickets[i]
		if cur.ID != exp.ID || cur.Sequence != exp.Sequence || cur.RunID != exp.RunID ||
			cur.WorkerID != exp.WorkerID || cur.OwnerPID != exp.OwnerPID ||
			cur.OwnerCreateTime != exp.OwnerCreateTime || cur.State != exp.State {
			t.Fatalf("ticket[%d] changed: %+v -> %+v", i, exp, cur)
		}
	}
}

// TestPrepareDuplicateAgainstExisting fails when generated ticket IDs clash
// with a pre-existing retained ticket.
func TestPrepareDuplicateAgainstExisting(t *testing.T) {
	root := t.TempDir()
	owner := restoreGateSeams(t, 5000, 5000000, map[int]pidResp{
		5000: {exists: true, createTime: 5000000},
	})

	futureID := "pre-exist"
	st := state{
		SchemaVersion: 1, NextSequence: 2,
		Tickets: []ticket{{
			ID: futureID, Sequence: 1, RunID: "old", WorkerID: 1,
			OwnerPID: owner.PID, OwnerCreateTime: owner.CreateTime, State: ticketQueued,
		}},
	}
	if err := saveState(root, st); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	g, err := Open(root, 1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	prev := newTicketID
	var mu sync.Mutex
	newTicketID = func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		return futureID, nil
	}
	defer func() { newTicketID = prev }()

	if _, err := g.Prepare([]Request{{RunID: "new", WorkerID: 2}}); err == nil {
		t.Fatal("Prepare(dup against existing): got nil, want error")
	}

	stCheck := readStateForTest(t, root)
	assertTicketCount(t, stCheck, 1)
	if stCheck.Tickets[0].ID != futureID {
		t.Fatalf("ticket ID=%q want %q", stCheck.Tickets[0].ID, futureID)
	}
}

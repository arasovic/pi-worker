//go:build darwin || linux

package admission

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// pidResp is a scripted process-table response for tests.
type pidResp struct {
	exists        bool
	existsErr     error
	createTime    int64
	createTimeErr error
}

// errTestSentinel is an unreachable sentinel for error comparisons.
var errTestSentinel = os.ErrInvalid

// restoreGateSeams installs deterministic seams for Gate and all its
// dependencies. It returns the owner identity matching the seams and
// restores every global in t.Cleanup.
func restoreGateSeams(t *testing.T, ownerPID int, ownerCreateTime int64, pidLookup map[int]pidResp) ownerIdentity {
	t.Helper()
	origGetpid := ownerGetpid
	origPidExists := pidExists
	origPidCreateTime := pidCreateTime
	origNewTicketID := newTicketID
	origPollInterval := pollInterval
	t.Cleanup(func() {
		ownerGetpid = origGetpid
		pidExists = origPidExists
		pidCreateTime = origPidCreateTime
		newTicketID = origNewTicketID
		pollInterval = origPollInterval
	})

	ownerGetpid = func() int { return ownerPID }
	pidExists = func(pid int) (bool, error) {
		if r, ok := pidLookup[pid]; ok {
			return r.exists, r.existsErr
		}
		return false, nil
	}
	pidCreateTime = func(pid int) (int64, error) {
		if r, ok := pidLookup[pid]; ok {
			return r.createTime, r.createTimeErr
		}
		return 0, nil
	}
	pollInterval = 10 * time.Millisecond

	return ownerIdentity{PID: ownerPID, CreateTime: ownerCreateTime}
}

// installSequentialTicketIDs installs deterministic sequential ticket IDs
// and returns a function that returns how many IDs have been consumed.
func installSequentialTicketIDs(t *testing.T, prefix string) func() int {
	t.Helper()
	orig := newTicketID
	var mu sync.Mutex
	n := 0
	t.Cleanup(func() { newTicketID = orig })
	newTicketID = func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		n++
		return prefix + padInt(n, 6), nil
	}
	return func() int { mu.Lock(); defer mu.Unlock(); return n }
}

func padInt(n, width int) string {
	s := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		s[i] = byte('0' + n%10)
		n /= 10
	}
	return string(s)
}

// enqueueAndWait is a test convenience that calls Enqueue then Wait.
// Production code must use Enqueue and Wait as separate two-phase calls.
func enqueueAndWait(ctx context.Context, g *Gate, req Request) (*Lease, error) {
	ticket, err := g.Enqueue(req)
	if err != nil {
		return nil, err
	}
	return ticket.Wait(ctx)
}

func readStateForTest(t *testing.T, root string) state {
	t.Helper()
	st, err := loadState(root)
	if err != nil {
		t.Fatalf("readStateForTest: loadState: %v", err)
	}
	return st
}

func writeRawState(t *testing.T, root, content string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "state.json"), []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile state.json: %v", err)
	}
}

func waitForCount(t *testing.T, root string, want int) state {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		st := readStateForTest(t, root)
		if len(st.Tickets) == want {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("waitForCount: got %d tickets after 1s, want %d", len(st.Tickets), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForTicketState(t *testing.T, root, ticketID, wantState string) state {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		st := readStateForTest(t, root)
		for _, tk := range st.Tickets {
			if tk.ID == ticketID && tk.State == wantState {
				return st
			}
		}
		if time.Now().After(deadline) {
			st := readStateForTest(t, root)
			t.Fatalf("waitForTicketState: ticket %q not in state %q after 1s; tickets=%v", ticketID, wantState, st.Tickets)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForTicketRemoved(t *testing.T, root, ticketID string) state {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		st := readStateForTest(t, root)
		found := false
		for _, tk := range st.Tickets {
			if tk.ID == ticketID {
				found = true
				break
			}
		}
		if !found {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("waitForTicketRemoved: ticket %q still present after 1s", ticketID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertNoTickets(t *testing.T, root string) {
	t.Helper()
	st := readStateForTest(t, root)
	if len(st.Tickets) != 0 {
		t.Fatalf("assertNoTickets: got %d tickets, want 0; tickets=%v", len(st.Tickets), st.Tickets)
	}
}

func assertTicketCount(t *testing.T, st state, want int) {
	t.Helper()
	if len(st.Tickets) != want {
		t.Fatalf("assertTicketCount: got %d, want %d; tickets=%v", len(st.Tickets), want, st.Tickets)
	}
}

func assertNextSequence(t *testing.T, st state, want int) {
	t.Helper()
	if st.NextSequence != want {
		t.Fatalf("assertNextSequence: got %d, want %d", st.NextSequence, want)
	}
}

// testGateOpts configures a test gate.
type testGateOpts struct {
	pid        int
	createTime int64
	pidLookup  map[int]pidResp
}

// openGateForTest creates a gate with deterministic seams and opens it.
func openGateForTest(t *testing.T, root string, maxLive int, opts ...func(*testGateOpts)) (*Gate, ownerIdentity) {
	t.Helper()
	o := &testGateOpts{pid: 5000, createTime: 5000000, pidLookup: map[int]pidResp{
		5000: {exists: true, createTime: 5000000},
	}}
	for _, fn := range opts {
		fn(o)
	}
	owner := restoreGateSeams(t, o.pid, o.createTime, o.pidLookup)
	g, err := Open(root, maxLive)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return g, owner
}

func withOwnerPID(pid int, ct int64) func(*testGateOpts) {
	return func(o *testGateOpts) {
		o.pid = pid
		o.createTime = ct
		o.pidLookup = map[int]pidResp{
			pid: {exists: true, createTime: ct},
		}
	}
}

func withExtraPIDLookup(extra map[int]pidResp) func(*testGateOpts) {
	return func(o *testGateOpts) {
		for pid, resp := range extra {
			o.pidLookup[pid] = resp
		}
	}
}

// --- tests ---

func TestGateOpenRejectsEmptyRoot(t *testing.T) {
	restoreGateSeams(t, 5000, 5000000, map[int]pidResp{
		5000: {exists: true, createTime: 5000000},
	})
	_, err := Open("", 1)
	if err == nil {
		t.Fatal("Open(\"\") = nil, want error")
	}
}

func TestGateOpenRejectsNonPositiveMaxLive(t *testing.T) {
	restoreGateSeams(t, 5000, 5000000, map[int]pidResp{
		5000: {exists: true, createTime: 5000000},
	})
	root := t.TempDir()
	for _, max := range []int{0, -1, -100} {
		_, err := Open(root, max)
		if err == nil {
			t.Errorf("Open(%s, %d) = nil, want error", root, max)
		}
	}
}

func TestGateOpenFailsClosedCorruptState(t *testing.T) {
	restoreGateSeams(t, 5000, 5000000, map[int]pidResp{
		5000: {exists: true, createTime: 5000000},
	})
	root := t.TempDir()
	writeRawState(t, root, `corrupt{json`)
	_, err := Open(root, 1)
	if err == nil {
		t.Fatal("Open(corrupt) = nil, want error")
	}
	raw, rerr := os.ReadFile(filepath.Join(root, "state.json"))
	if rerr != nil {
		t.Fatalf("ReadFile: %v", rerr)
	}
	if string(raw) != `corrupt{json` {
		t.Fatalf("state.json was replaced: %q", string(raw))
	}
}

func TestGateOpenFailsClosedSymlinkedState(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping symlink test in short mode")
	}
	restoreGateSeams(t, 5000, 5000000, map[int]pidResp{
		5000: {exists: true, createTime: 5000000},
	})
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte(`{"schemaVersion":1,"nextSequence":1,"tickets":[]}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(root, "state.json")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	_, err := Open(root, 1)
	if err == nil {
		t.Fatal("Open(symlink) = nil, want error")
	}
}

func TestGateOpenCapturesPositiveOwner(t *testing.T) {
	restoreGateSeams(t, 7777, 7777000, map[int]pidResp{
		7777: {exists: true, createTime: 7777000},
	})
	root := t.TempDir()
	g, err := Open(root, 1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if g.owner.PID != 7777 {
		t.Errorf("gate owner PID = %d, want 7777", g.owner.PID)
	}
	if g.owner.CreateTime != 7777000 {
		t.Errorf("gate owner CreateTime = %d, want 7777000", g.owner.CreateTime)
	}
}

func TestQueueTicketWaitAlreadyCancelled(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 1)
	installSequentialTicketIDs(t, "QW-")

	// Phase 1: Enqueue creates a durably queued ticket.
	qt, err := g.Enqueue(Request{RunID: "r1", WorkerID: 1})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	st := readStateForTest(t, root)
	assertTicketCount(t, st, 1)
	assertNextSequence(t, st, 2)
	if st.Tickets[0].State != ticketQueued {
		t.Fatalf("initial state = %q, want %q", st.Tickets[0].State, ticketQueued)
	}

	// Phase 2: Wait with already-cancelled context returns nil Lease
	// and context.Canceled; the queued ticket is removed by Cancel
	// inside Wait, but NextSequence never rolls back.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	lease, err := qt.Wait(ctx)
	if lease != nil {
		t.Fatal("Wait(cancelled) returned non-nil lease")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}

	st = readStateForTest(t, root)
	assertTicketCount(t, st, 0)
	assertNextSequence(t, st, 2)
}

func TestGateAcquireAndRelease(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 1)
	nIDs := installSequentialTicketIDs(t, "T-")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lease, err := enqueueAndWait(ctx, g, Request{RunID: "run-42", WorkerID: 7})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if lease == nil {
		t.Fatal("Acquire returned nil lease")
	}
	if got := nIDs(); got != 1 {
		t.Fatalf("ticket IDs consumed = %d, want 1", got)
	}

	st := readStateForTest(t, root)
	assertTicketCount(t, st, 1)
	assertNextSequence(t, st, 2)

	tk := st.Tickets[0]
	if tk.ID != "T-000001" {
		t.Errorf("ticket ID = %q, want T-000001", tk.ID)
	}
	if tk.Sequence != 1 {
		t.Errorf("sequence = %d, want 1", tk.Sequence)
	}
	if tk.RunID != "run-42" {
		t.Errorf("RunID = %q, want run-42", tk.RunID)
	}
	if tk.WorkerID != 7 {
		t.Errorf("WorkerID = %d, want 7", tk.WorkerID)
	}
	if tk.State != ticketLeased {
		t.Errorf("State = %q, want %q", tk.State, ticketLeased)
	}
	if tk.OwnerPID != g.owner.PID || tk.OwnerCreateTime != g.owner.CreateTime {
		t.Errorf("owner = {%d, %d}, want gate owner", tk.OwnerPID, tk.OwnerCreateTime)
	}

	if err := lease.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	st = readStateForTest(t, root)
	assertTicketCount(t, st, 0)
	assertNextSequence(t, st, 2)

	// Repeated Release is idempotent.
	if err := lease.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	st = readStateForTest(t, root)
	assertTicketCount(t, st, 0)
	assertNextSequence(t, st, 2)
}

func TestGateMaxLive1SecondAcquireQueued(t *testing.T) {
	root := t.TempDir()
	g, err := Open(root, 1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	lease1, err := enqueueAndWait(ctx, g, Request{RunID: "r1", WorkerID: 1})
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	type acqResult struct {
		lease *Lease
		err   error
	}
	acq2Done := make(chan acqResult, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	var secondLease *Lease
	t.Cleanup(func() {
		cancel()
		if lease1 != nil {
			_ = lease1.Release()
		}
		wg.Wait()
		if secondLease != nil {
			_ = secondLease.Release()
		}
	})
	go func() {
		defer wg.Done()
		l, e := enqueueAndWait(ctx, g, Request{RunID: "r2", WorkerID: 2})
		acq2Done <- acqResult{l, e}
	}()

	st := waitForCount(t, root, 2)
	if st.Tickets[1].State != ticketQueued {
		t.Fatalf("second ticket state = %q, want %q", st.Tickets[1].State, ticketQueued)
	}
	if st.Tickets[1].RunID != "r2" {
		t.Errorf("second ticket RunID = %q, want r2", st.Tickets[1].RunID)
	}

	if err := lease1.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	var r2 acqResult
	select {
	case r2 = <-acq2Done:
	case <-time.After(time.Second):
		t.Fatal("second Acquire did not complete after release")
	}
	if r2.err != nil {
		t.Fatalf("second Acquire error: %v", r2.err)
	}
	if r2.lease == nil {
		t.Fatal("second Acquire returned nil lease")
	}
	secondLease = r2.lease
	if err := r2.lease.Release(); err != nil {
		t.Fatalf("Release second: %v", err)
	}
	secondLease = nil
}

func TestGateFIFOStrictOrdering(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 1)
	installSequentialTicketIDs(t, "FO-")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Phase 1: Enqueue r0, then Wait to acquire a lease (hold maxLive=1).
	qt0, err := g.Enqueue(Request{RunID: "r0", WorkerID: 1})
	if err != nil {
		t.Fatalf("Enqueue r0: %v", err)
	}
	lease0, err := qt0.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait r0: %v", err)
	}
	if lease0 == nil {
		t.Fatal("Wait r0 returned nil lease")
	}

	// Phase 1: Synchronously enqueue rA then rB before starting any Wait.
	qtA, err := g.Enqueue(Request{RunID: "rA", WorkerID: 10})
	if err != nil {
		t.Fatalf("Enqueue rA: %v", err)
	}
	qtB, err := g.Enqueue(Request{RunID: "rB", WorkerID: 20})
	if err != nil {
		t.Fatalf("Enqueue rB: %v", err)
	}

	// Inspect durable state before Wait starts: r0 leased, rA/rB queued,
	// A sequence < B, NextSequence=4.
	st := readStateForTest(t, root)
	assertNextSequence(t, st, 4)
	assertTicketCount(t, st, 3)
	if st.Tickets[0].RunID != "r0" || st.Tickets[0].State != ticketLeased {
		t.Fatalf("r0: RunID=%q State=%q, want r0/%s", st.Tickets[0].RunID, st.Tickets[0].State, ticketLeased)
	}
	if st.Tickets[1].RunID != "rA" || st.Tickets[1].State != ticketQueued {
		t.Fatalf("rA: RunID=%q State=%q, want rA/%s", st.Tickets[1].RunID, st.Tickets[1].State, ticketQueued)
	}
	if st.Tickets[2].RunID != "rB" || st.Tickets[2].State != ticketQueued {
		t.Fatalf("rB: RunID=%q State=%q, want rB/%s", st.Tickets[2].RunID, st.Tickets[2].State, ticketQueued)
	}
	if st.Tickets[1].Sequence >= st.Tickets[2].Sequence {
		t.Fatalf("rA sequence %d must be < rB sequence %d", st.Tickets[1].Sequence, st.Tickets[2].Sequence)
	}

	// Phase 2: Start A/B Wait goroutines concurrently.
	type result struct {
		name  string
		lease *Lease
		err   error
	}
	results := make(chan result, 2)

	var wg sync.WaitGroup
	var rA, rB result
	wg.Add(2)
	t.Cleanup(func() {
		cancel()
		if lease0 != nil {
			_ = lease0.Release()
		}
		wg.Wait()
		if rA.lease != nil {
			_ = rA.lease.Release()
		}
		if rB.lease != nil {
			_ = rB.lease.Release()
		}
	})

	go func() {
		defer wg.Done()
		l, e := qtA.Wait(ctx)
		results <- result{"A", l, e}
	}()
	go func() {
		defer wg.Done()
		l, e := qtB.Wait(ctx)
		results <- result{"B", l, e}
	}()

	// Release held lease → earlier queued (A) must be granted first.
	if err := lease0.Release(); err != nil {
		t.Fatalf("Release r0: %v", err)
	}

	// Collect both results with bounded timeout, releasing each lease immediately
	// so maxLive=1 can grant the second.
	order := make([]string, 0, 2)
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for len(order) < 2 {
		select {
		case r := <-results:
			order = append(order, r.name)
			switch r.name {
			case "A":
				rA = r
			case "B":
				rB = r
			}
			if r.lease != nil {
				_ = r.lease.Release()
			}
		case <-timer.C:
			cancel()
			if lease0 != nil {
				_ = lease0.Release()
			}
			wg.Wait()
			if rA.lease != nil {
				_ = rA.lease.Release()
			}
			if rB.lease != nil {
				_ = rB.lease.Release()
			}
			t.Fatal("timed out waiting for both Wait results")
		}
	}

	// Both goroutines have returned; assert ordering.
	if rA.err != nil {
		t.Fatalf("A Wait error: %v", rA.err)
	}
	if rB.err != nil {
		t.Fatalf("B Wait error: %v", rB.err)
	}
	if rA.lease == nil {
		t.Fatal("A Wait returned nil lease")
	}
	if rB.lease == nil {
		t.Fatal("B Wait returned nil lease")
	}
	if len(order) != 2 || order[0] != "A" || order[1] != "B" {
		t.Fatalf("unexpected arrival order: %v, want [A B]", order)
	}

	// After all releases, state has no tickets and NextSequence remains 4.
	st = readStateForTest(t, root)
	assertTicketCount(t, st, 0)
	assertNextSequence(t, st, 4)
}

func TestGateCancelQueuedAcquire(t *testing.T) {
	root := t.TempDir()
	g, err := Open(root, 1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r1Lease, err := enqueueAndWait(ctx, g, Request{RunID: "r1", WorkerID: 1})
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	type acqResult struct {
		lease *Lease
		err   error
	}
	bDone := make(chan acqResult, 1)
	go func() {
		l, e := enqueueAndWait(ctx, g, Request{RunID: "r2", WorkerID: 2})
		bDone <- acqResult{l, e}
	}()

	st := waitForCount(t, root, 2)
	bID := st.Tickets[1].ID
	if st.Tickets[1].RunID != "r2" {
		t.Fatalf("queued ticket RunID = %q, want r2", st.Tickets[1].RunID)
	}

	// Cancel the queued Acquire.
	cancel()

	var r2 acqResult
	select {
	case r2 = <-bDone:
	case <-time.After(time.Second):
		t.Fatal("cancelled Acquire did not return after 1s")
	}
	if r2.lease != nil {
		t.Fatal("cancelled Acquire returned non-nil lease")
	}
	if !errors.Is(r2.err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", r2.err)
	}

	// Only the queued ticket was removed; live lease remains.
	waitForTicketRemoved(t, root, bID)
	st = readStateForTest(t, root)
	assertTicketCount(t, st, 1)
	if st.Tickets[0].State != ticketLeased {
		t.Errorf("live ticket state = %q, want %q", st.Tickets[0].State, ticketLeased)
	}
	if st.Tickets[0].RunID != "r1" {
		t.Errorf("live ticket RunID = %q, want r1", st.Tickets[0].RunID)
	}

	if err := r1Lease.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestGateReapStaleAbsentPID(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 2)
	installSequentialTicketIDs(t, "R-")

	stale := ticket{
		ID: "stale-absent", Sequence: 1, RunID: "old-run",
		WorkerID: 1, OwnerPID: 9999, OwnerCreateTime: 9999000,
		State: ticketQueued,
	}
	if err := saveState(root, state{
		SchemaVersion: 1, NextSequence: 2,
		Tickets: []ticket{stale},
	}); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	lease, err := enqueueAndWait(context.Background(), g, Request{RunID: "new-run", WorkerID: 2})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if lease == nil {
		t.Fatal("Acquire returned nil lease")
	}

	st := readStateForTest(t, root)
	assertTicketCount(t, st, 1)
	if st.Tickets[0].RunID != "new-run" {
		t.Errorf("remaining ticket RunID = %q, want new-run", st.Tickets[0].RunID)
	}
	if st.Tickets[0].State != ticketLeased {
		t.Errorf("remaining ticket State = %q, want %q", st.Tickets[0].State, ticketLeased)
	}
}

func TestGateReapStalePIDReuse(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 2)
	installSequentialTicketIDs(t, "S-")

	stale := ticket{
		ID: "stale-reused", Sequence: 1, RunID: "old-run",
		WorkerID: 1, OwnerPID: 8888, OwnerCreateTime: 8888000,
		State: ticketLeased,
	}
	if err := saveState(root, state{
		SchemaVersion: 1, NextSequence: 2,
		Tickets: []ticket{stale},
	}); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	lease, err := enqueueAndWait(context.Background(), g, Request{RunID: "new-run", WorkerID: 2})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if lease == nil {
		t.Fatal("Acquire returned nil lease")
	}

	st := readStateForTest(t, root)
	assertTicketCount(t, st, 1)
	if st.Tickets[0].RunID != "new-run" {
		t.Errorf("remaining ticket RunID = %q, want new-run", st.Tickets[0].RunID)
	}
}

func TestGateUncertainOwnerRemainsInState(t *testing.T) {
	root := t.TempDir()

	// Gate owner PID 5000 succeeds on Open. Uncertain ticket uses PID 5555
	// where pidCreateTime returns an error → ownerUncertain → not reaped.
	uncertainPID := 5555
	rawState := `{"schemaVersion":1,"nextSequence":2,"tickets":[{"id":"uncertain","sequence":1,"runId":"uncertain-run","workerId":1,"ownerPid":5555,"ownerCreateTime":5555000,"state":"queued"}]}` + "\n"
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "state.json"), []byte(rawState), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	owner := restoreGateSeams(t, 5000, 5000000, map[int]pidResp{
		5000:         {exists: true, createTime: 5000000},
		uncertainPID: {exists: true, createTimeErr: errors.New("lookup failed")}, // uncertain
	})
	_ = owner

	g, err := Open(root, 1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	installSequentialTicketIDs(t, "U-")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bDone := make(chan error, 1)
	go func() {
		_, e := enqueueAndWait(ctx, g, Request{RunID: "new-run", WorkerID: 2})
		bDone <- e
	}()

	waitForCount(t, root, 2)
	st := readStateForTest(t, root)
	if st.Tickets[0].ID != "uncertain" {
		t.Errorf("first ticket ID = %q, want uncertain", st.Tickets[0].ID)
	}
	if st.Tickets[0].State != ticketQueued {
		t.Errorf("uncertain ticket State = %q, want queued", st.Tickets[0].State)
	}

	cancel()
	select {
	case <-bDone:
	case <-time.After(time.Second):
		t.Fatal("blocked Acquire did not return after cancel")
	}

	st = readStateForTest(t, root)
	found := false
	for _, tk := range st.Tickets {
		if tk.ID == "uncertain" {
			found = true
		}
	}
	if !found {
		t.Error("uncertain ticket was removed (must not be reaped)")
	}
}

func TestGateUncertainLookupErrorNotReaped(t *testing.T) {
	root := t.TempDir()

	// Stale ticket: PID 6000 with createTime 6000000. During reaping,
	// pidCreateTime for PID 6000 returns an error → uncertain → not reaped.
	stale := ticket{
		ID: "lookup-err", Sequence: 1, RunID: "run-err",
		WorkerID: 1, OwnerPID: 6000, OwnerCreateTime: 6000000,
		State: ticketQueued,
	}

	// Gate owner PID 5000.
	owner := restoreGateSeams(t, 5000, 5000000, map[int]pidResp{
		5000: {exists: true, createTime: 5000000},
		6000: {exists: true, createTimeErr: errors.New("perm denied")}, // uncertain
	})
	_ = owner

	g, err := Open(root, 1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	installSequentialTicketIDs(t, "U-")

	if err := saveState(root, state{
		SchemaVersion: 1, NextSequence: 2,
		Tickets: []ticket{stale},
	}); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bDone := make(chan error, 1)
	go func() {
		_, e := enqueueAndWait(ctx, g, Request{RunID: "run-new", WorkerID: 2})
		bDone <- e
	}()

	waitForCount(t, root, 2)
	st := readStateForTest(t, root)
	found := false
	for _, tk := range st.Tickets {
		if tk.ID == "lookup-err" {
			found = true
			if tk.State != ticketQueued {
				t.Errorf("uncertain ticket state changed to %q", tk.State)
			}
		}
	}
	if !found {
		t.Error("uncertain ticket was removed (must not be reaped on lookup error)")
	}

	cancel()
	select {
	case <-bDone:
	case <-time.After(time.Second):
		t.Fatal("blocked Acquire did not return after cancel")
	}
}

func TestGateUncertainInvalidCreateTimeNotReaped(t *testing.T) {
	root := t.TempDir()

	// Stale ticket: PID 6100 with createTime 6100000. During reaping,
	// pidCreateTime returns 0 (invalid) → uncertain → not reaped.
	stale := ticket{
		ID: "bad-ct", Sequence: 1, RunID: "run-badct",
		WorkerID: 1, OwnerPID: 6100, OwnerCreateTime: 6100000,
		State: ticketQueued,
	}

	owner := restoreGateSeams(t, 5000, 5000000, map[int]pidResp{
		5000: {exists: true, createTime: 5000000},
		6100: {exists: true, createTime: 0}, // invalid createTime → uncertain
	})
	_ = owner

	g, err := Open(root, 1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	installSequentialTicketIDs(t, "U-")

	if err := saveState(root, state{
		SchemaVersion: 1, NextSequence: 2,
		Tickets: []ticket{stale},
	}); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bDone := make(chan error, 1)
	go func() {
		_, e := enqueueAndWait(ctx, g, Request{RunID: "run-new", WorkerID: 2})
		bDone <- e
	}()

	waitForCount(t, root, 2)
	st := readStateForTest(t, root)
	found := false
	for _, tk := range st.Tickets {
		if tk.ID == "bad-ct" {
			found = true
		}
	}
	if !found {
		t.Error("ticket with invalid createTime was removed (must not be reaped)")
	}

	cancel()
	select {
	case <-bDone:
	case <-time.After(time.Second):
		t.Fatal("blocked Acquire did not return after cancel")
	}
}

func TestGateCorruptStateDuringAcquire(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 1)
	installSequentialTicketIDs(t, "X-")

	writeRawState(t, root, `{bad json`)

	_, err := enqueueAndWait(context.Background(), g, Request{RunID: "r1", WorkerID: 1})
	if err == nil {
		t.Fatal("Acquire(corrupt) = nil, want error")
	}
	raw, rerr := os.ReadFile(filepath.Join(root, "state.json"))
	if rerr != nil {
		t.Fatalf("ReadFile: %v", rerr)
	}
	if string(raw) != `{bad json` {
		t.Fatalf("state.json was replaced: %q", string(raw))
	}
}

func TestGateCorruptStateDuringRelease(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 1)
	installSequentialTicketIDs(t, "Y-")

	ctx := context.Background()
	lease, err := enqueueAndWait(ctx, g, Request{RunID: "r1", WorkerID: 1})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	writeRawState(t, root, `}`)

	err = lease.Release()
	if err == nil {
		t.Fatal("Release(corrupt) = nil, want error")
	}
	raw, rerr := os.ReadFile(filepath.Join(root, "state.json"))
	if rerr != nil {
		t.Fatalf("ReadFile: %v", rerr)
	}
	if string(raw) != `}` {
		t.Fatalf("state.json was replaced: %q", string(raw))
	}
}

func TestGateDuplicateTicketIDFails(t *testing.T) {
	root := t.TempDir()
	restoreGateSeams(t, 5000, 5000000, map[int]pidResp{
		5000: {exists: true, createTime: 5000000},
	})

	// Force newTicketID to always return the same ID.
	var mu sync.Mutex
	newTicketID = func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		return "dup-id", nil
	}
	pollInterval = 10 * time.Millisecond

	g, err := Open(root, 2)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lease, err := enqueueAndWait(ctx, g, Request{RunID: "r1", WorkerID: 1})
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	_ = lease

	// Second Enqueue fails: state.json already has "dup-id" → duplicate
	// ID rejected by saveState via validateState.
	_, err = g.Enqueue(Request{RunID: "r2", WorkerID: 2})
	if err == nil {
		t.Fatal("second Enqueue(dup) = nil, want error")
	}

	st := readStateForTest(t, root)
	assertTicketCount(t, st, 1)
	if st.Tickets[0].ID != "dup-id" {
		t.Errorf("surviving ticket ID = %q, want dup-id", st.Tickets[0].ID)
	}
}

func TestGateSequenceExhaustion(t *testing.T) {
	root := t.TempDir()
	restoreGateSeams(t, 5000, 5000000, map[int]pidResp{
		5000: {exists: true, createTime: 5000000},
	})
	installSequentialTicketIDs(t, "E-")
	pollInterval = 10 * time.Millisecond

	if err := saveState(root, state{
		SchemaVersion: 1, NextSequence: math.MaxInt,
		Tickets: []ticket{},
	}); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	g, err := Open(root, 1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	_, err = g.Enqueue(Request{RunID: "r1", WorkerID: 1})
	if err == nil {
		t.Fatal("Enqueue(exhausted) = nil, want error")
	}

	st := readStateForTest(t, root)
	assertNextSequence(t, st, math.MaxInt)
	assertTicketCount(t, st, 0)
}

func TestGateOpenRejectsCorruptStateThenAcquireFailsClosed(t *testing.T) {
	root := t.TempDir()
	restoreGateSeams(t, 5000, 5000000, map[int]pidResp{
		5000: {exists: true, createTime: 5000000},
	})
	writeRawState(t, root, `not json`)

	_, err := Open(root, 1)
	if err == nil {
		t.Fatal("Open(corrupt) = nil, want error")
	}
	raw, rerr := os.ReadFile(filepath.Join(root, "state.json"))
	if rerr != nil {
		t.Fatalf("ReadFile: %v", rerr)
	}
	if string(raw) != `not json` {
		t.Fatalf("state.json changed: %q", string(raw))
	}
}

func TestGateAcquireThenCancelThenReleaseLiveLease(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 1)
	installSequentialTicketIDs(t, "Z-")

	ctx, cancel := context.WithCancel(context.Background())

	lease, err := enqueueAndWait(ctx, g, Request{RunID: "r1", WorkerID: 1})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	bDone := make(chan error, 1)
	go func() {
		_, e := enqueueAndWait(ctx, g, Request{RunID: "r2", WorkerID: 2})
		bDone <- e
	}()
	waitForCount(t, root, 2)

	cancel()
	select {
	case <-bDone:
	case <-time.After(time.Second):
		t.Fatal("cancelled Acquire did not return")
	}

	st := readStateForTest(t, root)
	assertTicketCount(t, st, 1)

	if err := lease.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	st = readStateForTest(t, root)
	assertTicketCount(t, st, 0)
}

func TestGateUncertainOwnerBlocksBothAcquires(t *testing.T) {
	root := t.TempDir()

	// Gate owner PID 5000 succeeds. Uncertain ticket uses PID 5555 where
	// pidCreateTime errors → ownerUncertain → not reaped → blocks.
	uncertainPID := 5555
	rawState := `{"schemaVersion":1,"nextSequence":2,"tickets":[{"id":"uncertain-zero","sequence":1,"runId":"run-unc","workerId":1,"ownerPid":5555,"ownerCreateTime":5555000,"state":"queued"}]}` + "\n"
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "state.json"), []byte(rawState), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	restoreGateSeams(t, 5000, 5000000, map[int]pidResp{
		5000:         {exists: true, createTime: 5000000},
		uncertainPID: {exists: true, createTimeErr: errors.New("lookup failed")},
	})

	g, err := Open(root, 1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	installSequentialTicketIDs(t, "U-")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	aDone := make(chan error, 1)
	bDone := make(chan error, 1)
	go func() {
		_, e := enqueueAndWait(ctx, g, Request{RunID: "rA", WorkerID: 10})
		aDone <- e
	}()
	go func() {
		_, e := enqueueAndWait(ctx, g, Request{RunID: "rB", WorkerID: 20})
		bDone <- e
	}()

	waitForCount(t, root, 3)

	// Neither Acquire can proceed because the uncertain ticket is first.
	time.Sleep(50 * time.Millisecond)
	st := readStateForTest(t, root)
	for _, tk := range st.Tickets {
		if tk.ID != "uncertain-zero" && tk.State == ticketLeased {
			t.Errorf("non-uncertain ticket %q was leased", tk.ID)
		}
	}

	cancel()
	select {
	case <-aDone:
	case <-time.After(time.Second):
		t.Fatal("a Acquire did not return after cancel")
	}
	select {
	case <-bDone:
	case <-time.After(time.Second):
		t.Fatal("b Acquire did not return after cancel")
	}

	st = readStateForTest(t, root)
	found := false
	for _, tk := range st.Tickets {
		if tk.ID == "uncertain-zero" {
			found = true
		}
	}
	if !found {
		t.Error("uncertain ticket removed after cancel")
	}
}

func TestGateAcquireAndReleaseTwoLeases(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 2)
	nIDs := installSequentialTicketIDs(t, "L-")

	ctx := context.Background()
	l1, err := enqueueAndWait(ctx, g, Request{RunID: "r1", WorkerID: 1})
	if err != nil {
		t.Fatalf("Acquire 1: %v", err)
	}
	l2, err := enqueueAndWait(ctx, g, Request{RunID: "r2", WorkerID: 2})
	if err != nil {
		t.Fatalf("Acquire 2: %v", err)
	}

	if got := nIDs(); got != 2 {
		t.Fatalf("IDs consumed = %d, want 2", got)
	}

	st := readStateForTest(t, root)
	assertTicketCount(t, st, 2)
	assertNextSequence(t, st, 3)
	for _, tk := range st.Tickets {
		if tk.State != ticketLeased {
			t.Errorf("ticket %q state = %q, want %q", tk.ID, tk.State, ticketLeased)
		}
	}

	if err := l1.Release(); err != nil {
		t.Fatalf("Release 1: %v", err)
	}
	st = readStateForTest(t, root)
	assertTicketCount(t, st, 1)

	if err := l2.Release(); err != nil {
		t.Fatalf("Release 2: %v", err)
	}
	st = readStateForTest(t, root)
	assertTicketCount(t, st, 0)
	assertNextSequence(t, st, 3)
}

func TestGateLeaseIdempotentThenRelease(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 1)
	installSequentialTicketIDs(t, "I-")

	lease, err := enqueueAndWait(context.Background(), g, Request{RunID: "r1", WorkerID: 1})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	if err := lease.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	assertNoTickets(t, root)

	if err := lease.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	assertNoTickets(t, root)
}

func TestGateEnqueueRunIDEmpty(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 1)

	_, err := g.Enqueue(Request{RunID: "", WorkerID: 1})
	if err == nil {
		t.Fatal("Enqueue(empty RunID) = nil, want error")
	}
}

func TestGateEnqueueWorkerIDNotPositive(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 1)

	for _, wid := range []int{0, -1} {
		_, err := g.Enqueue(Request{RunID: "r1", WorkerID: wid})
		if err == nil {
			t.Errorf("Enqueue(WorkerID=%d) = nil, want error", wid)
		}
	}
}

func TestGateReleaseRetryAfterTransientFailure(t *testing.T) {
	root := t.TempDir()
	restoreGateSeams(t, 5000, 5000000, map[int]pidResp{
		5000: {exists: true, createTime: 5000000},
	})
	installSequentialTicketIDs(t, "RR-")

	g, _ := openGateForTest(t, root, 1)

	// 1. Acquire one lease.
	lease, err := enqueueAndWait(context.Background(), g, Request{RunID: "retry-run", WorkerID: 1})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if lease == nil {
		t.Fatal("Acquire returned nil lease")
	}

	// 2. Save exact valid state.json bytes.
	validBytes, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// 3. Replace state.json with malformed JSON so first Release fails.
	if err := os.WriteFile(filepath.Join(root, "state.json"), []byte(`{bad json`), 0o600); err != nil {
		t.Fatalf("WriteFile corrupt: %v", err)
	}
	if err := lease.Release(); err == nil {
		t.Fatal("Release on corrupt state = nil, want error")
	}

	// 4. Restore the exact valid bytes.
	if err := os.WriteFile(filepath.Join(root, "state.json"), validBytes, 0o600); err != nil {
		t.Fatalf("WriteFile restore: %v", err)
	}

	// 5. Second Release succeeds.
	if err := lease.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}

	// 6. Third Release is idempotent.
	if err := lease.Release(); err != nil {
		t.Fatalf("third Release: %v", err)
	}

	// 7. Final state has no tickets and unchanged nextSequence.
	st := readStateForTest(t, root)
	assertNoTickets(t, root)
	assertNextSequence(t, st, 2)
}

func TestGateRemoveQueuedTicketExactCleanupAndStalePersistence(t *testing.T) {
	root := t.TempDir()
	owner := restoreGateSeams(t, 5000, 5000000, map[int]pidResp{
		5000: {exists: true, createTime: 5000000},
	})
	installSequentialTicketIDs(t, "RQ-")

	// Preload: leased ticket (owner match), target queued ticket (owner match),
	// stale unrelated ticket (different owner, absent PID).
	leased := ticket{
		ID: "leased-1", Sequence: 1, RunID: "leased-run",
		WorkerID: 1, OwnerPID: owner.PID, OwnerCreateTime: owner.CreateTime,
		State: ticketLeased,
	}
	target := ticket{
		ID: "target-q", Sequence: 2, RunID: "target-run",
		WorkerID: 2, OwnerPID: owner.PID, OwnerCreateTime: owner.CreateTime,
		State: ticketQueued,
	}
	stale := ticket{
		ID: "stale-1", Sequence: 3, RunID: "stale-run",
		WorkerID: 3, OwnerPID: 9999, OwnerCreateTime: 9999000,
		State: ticketQueued,
	}
	if err := saveState(root, state{
		SchemaVersion: 1, NextSequence: 4,
		Tickets: []ticket{leased, target, stale},
	}); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	g, err := Open(root, 2)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Call removeQueuedTicket for the target queued ticket.
	if err := g.removeQueuedTicket("target-q"); err != nil {
		t.Fatalf("removeQueuedTicket(target-q): %v", err)
	}

	// Target queued and stale are gone; leased remains; nextSequence unchanged.
	st := readStateForTest(t, root)
	assertNextSequence(t, st, 4)
	assertTicketCount(t, st, 1)
	if st.Tickets[0].ID != "leased-1" {
		t.Errorf("remaining ticket ID = %q, want leased-1", st.Tickets[0].ID)
	}
	if st.Tickets[0].State != ticketLeased {
		t.Errorf("remaining ticket state = %q, want %q", st.Tickets[0].State, ticketLeased)
	}

	// Calling removeQueuedTicket with the leased ticket ID must not remove it.
	if err := g.removeQueuedTicket("leased-1"); err != nil {
		t.Fatalf("removeQueuedTicket(leased-1): %v", err)
	}
	st = readStateForTest(t, root)
	assertTicketCount(t, st, 1)
	if st.Tickets[0].ID != "leased-1" {
		t.Errorf("after remove leased: ID = %q, want leased-1", st.Tickets[0].ID)
	}
}

func TestGateCanceledTryGrant(t *testing.T) {
	root := t.TempDir()
	owner := restoreGateSeams(t, 5000, 5000000, map[int]pidResp{
		5000: {exists: true, createTime: 5000000},
	})
	installSequentialTicketIDs(t, "CG-")

	// Preload: one matching-owner queued ticket eligible under maxLive=1.
	if err := saveState(root, state{
		SchemaVersion: 1, NextSequence: 2,
		Tickets: []ticket{{
			ID: "q-ticket", Sequence: 1, RunID: "cancel-run",
			WorkerID: 1, OwnerPID: owner.PID, OwnerCreateTime: owner.CreateTime,
			State: ticketQueued,
		}},
	}); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	g, err := Open(root, 1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Cancel context before calling tryGrant.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// tryGrant with canceled context: must return nil Lease and
	// errors.Is(err, context.Canceled).
	lease, err := g.tryGrant(ctx, "q-ticket", g.owner)
	if lease != nil {
		t.Fatal("tryGrant(canceled) returned non-nil lease")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("tryGrant(canceled) error = %v, want context.Canceled", err)
	}

	// Durable ticket remains queued (not leased).
	st := readStateForTest(t, root)
	assertTicketCount(t, st, 1)
	if st.Tickets[0].ID != "q-ticket" {
		t.Errorf("ticket ID = %q, want q-ticket", st.Tickets[0].ID)
	}
	if st.Tickets[0].State != ticketQueued {
		t.Errorf("ticket state = %q, want %q (still queued)", st.Tickets[0].State, ticketQueued)
	}

	// removeQueuedTicket cleans it.
	if err := g.removeQueuedTicket("q-ticket"); err != nil {
		t.Fatalf("removeQueuedTicket: %v", err)
	}
	assertNoTickets(t, root)
}

func TestGateOneGrantPerTransition(t *testing.T) {
	root := t.TempDir()
	owner := restoreGateSeams(t, 5000, 5000000, map[int]pidResp{
		5000: {exists: true, createTime: 5000000},
	})
	installSequentialTicketIDs(t, "OG-")

	// Preload three matching-owner queued tickets in sequence, maxLive=3.
	if err := saveState(root, state{
		SchemaVersion: 1, NextSequence: 4,
		Tickets: []ticket{
			{ID: "og-1", Sequence: 1, RunID: "run-1", WorkerID: 1,
				OwnerPID: owner.PID, OwnerCreateTime: owner.CreateTime, State: ticketQueued},
			{ID: "og-2", Sequence: 2, RunID: "run-2", WorkerID: 2,
				OwnerPID: owner.PID, OwnerCreateTime: owner.CreateTime, State: ticketQueued},
			{ID: "og-3", Sequence: 3, RunID: "run-3", WorkerID: 3,
				OwnerPID: owner.PID, OwnerCreateTime: owner.CreateTime, State: ticketQueued},
		},
	}); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	g, err := Open(root, 3)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// One direct tryGrant call for the first ticket returns its Lease.
	lease, err := g.tryGrant(ctx, "og-1", g.owner)
	if err != nil {
		t.Fatalf("tryGrant(og-1): %v", err)
	}
	if lease == nil {
		t.Fatal("tryGrant(og-1) returned nil lease")
	}

	// Inspect: exactly one ticket leased, two remain queued in order.
	st := readStateForTest(t, root)
	assertNextSequence(t, st, 4)
	assertTicketCount(t, st, 3)

	var leasedIDs, queuedIDs []string
	for _, tk := range st.Tickets {
		switch tk.State {
		case ticketLeased:
			leasedIDs = append(leasedIDs, tk.ID)
		case ticketQueued:
			queuedIDs = append(queuedIDs, tk.ID)
		}
	}
	if len(leasedIDs) != 1 || leasedIDs[0] != "og-1" {
		t.Fatalf("leased IDs = %v, want [og-1]", leasedIDs)
	}
	if len(queuedIDs) != 2 || queuedIDs[0] != "og-2" || queuedIDs[1] != "og-3" {
		t.Fatalf("queued IDs = %v, want [og-2 og-3]", queuedIDs)
	}

	// Release the returned Lease.
	if err := lease.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	assertTicketCount(t, readStateForTest(t, root), 2)

	// Clean the two queued tickets.
	if err := g.removeQueuedTicket("og-2"); err != nil {
		t.Fatalf("removeQueuedTicket(og-2): %v", err)
	}
	if err := g.removeQueuedTicket("og-3"); err != nil {
		t.Fatalf("removeQueuedTicket(og-3): %v", err)
	}
	assertNoTickets(t, root)
	assertNextSequence(t, readStateForTest(t, root), 4)
}

func TestQueueTicketSequentialEnqueueAndCancel(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 3)
	installSequentialTicketIDs(t, "SE-")

	// Enqueue three requests before any Wait; all are queued.
	qt1, err := g.Enqueue(Request{RunID: "r1", WorkerID: 1})
	if err != nil {
		t.Fatalf("Enqueue r1: %v", err)
	}
	qt2, err := g.Enqueue(Request{RunID: "r2", WorkerID: 2})
	if err != nil {
		t.Fatalf("Enqueue r2: %v", err)
	}
	qt3, err := g.Enqueue(Request{RunID: "r3", WorkerID: 3})
	if err != nil {
		t.Fatalf("Enqueue r3: %v", err)
	}

	// Assert durable worker/run order and sequences 1/2/3.
	st := readStateForTest(t, root)
	assertTicketCount(t, st, 3)
	assertNextSequence(t, st, 4)
	type entry struct {
		runID    string
		workerID int
		seq      int
	}
	var got []entry
	for _, tk := range st.Tickets {
		got = append(got, entry{tk.RunID, tk.WorkerID, tk.Sequence})
	}
	want := []entry{
		{"r1", 1, 1},
		{"r2", 2, 2},
		{"r3", 3, 3},
	}
	if len(got) != len(want) {
		t.Fatalf("ticket count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ticket[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Cancel all in reverse order.
	if err := qt3.Cancel(); err != nil {
		t.Fatalf("Cancel r3: %v", err)
	}
	if err := qt2.Cancel(); err != nil {
		t.Fatalf("Cancel r2: %v", err)
	}
	if err := qt1.Cancel(); err != nil {
		t.Fatalf("Cancel r1: %v", err)
	}

	// Final empty with NextSequence=4.
	st = readStateForTest(t, root)
	assertTicketCount(t, st, 0)
	assertNextSequence(t, st, 4)
}

func TestQueueTicketCancelIdempotent(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 1)
	installSequentialTicketIDs(t, "CI-")

	qt, err := g.Enqueue(Request{RunID: "r1", WorkerID: 1})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// First Cancel removes the queued ticket.
	if err := qt.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	assertNoTickets(t, root)

	// Second Cancel is idempotent: no state change.
	if err := qt.Cancel(); err != nil {
		t.Fatalf("second Cancel: %v", err)
	}
	assertNoTickets(t, root)

	// Wait after successful Cancel returns deterministic canceled error
	// without any state change.
	lease, err := qt.Wait(context.Background())
	if lease != nil {
		t.Fatal("Wait after Cancel returned non-nil lease")
	}
	if err == nil {
		t.Fatal("Wait after Cancel = nil, want error")
	}
	if got := err.Error(); got != "admission wait: ticket cancelled" {
		t.Fatalf("Wait after Cancel error = %q, want %q", got, "admission wait: ticket cancelled")
	}
	assertNoTickets(t, root)
}

func TestQueueTicketCancelAfterLeaseDoesNotRemoveRecord(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 1)
	installSequentialTicketIDs(t, "CL-")

	// Enqueue and Wait to get a Lease.
	qt, err := g.Enqueue(Request{RunID: "r1", WorkerID: 1})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	lease, err := qt.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if lease == nil {
		t.Fatal("Wait returned nil lease")
	}

	// Cancel after Lease is granted does not remove the leased record.
	if err := qt.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	st := readStateForTest(t, root)
	assertTicketCount(t, st, 1)
	if st.Tickets[0].State != ticketLeased {
		t.Fatalf("leased ticket state = %q, want %q", st.Tickets[0].State, ticketLeased)
	}

	// Release still succeeds.
	if err := lease.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	assertNoTickets(t, root)
}

func TestQueueTicketSecondWaitRejected(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 1)
	installSequentialTicketIDs(t, "SW-")

	qt, err := g.Enqueue(Request{RunID: "r1", WorkerID: 1})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// First Wait grants a Lease.
	lease, err := qt.Wait(context.Background())
	if err != nil {
		t.Fatalf("first Wait: %v", err)
	}
	if lease == nil {
		t.Fatal("first Wait returned nil lease")
	}

	// Second Wait is rejected before any durable state changes.
	lease2, err := qt.Wait(context.Background())
	if lease2 != nil {
		t.Fatal("second Wait returned non-nil lease")
	}
	if err == nil {
		t.Fatal("second Wait = nil, want error")
	}

	// First Wait/Lease remains valid: ticket still leased.
	st := readStateForTest(t, root)
	assertTicketCount(t, st, 1)
	if st.Tickets[0].State != ticketLeased {
		t.Fatalf("ticket state = %q, want %q", st.Tickets[0].State, ticketLeased)
	}

	// Release cleans up.
	if err := lease.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	assertNoTickets(t, root)
}

func TestQueueTicketWaitGrantNeverAllocatesSecondTicket(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 2)
	installSequentialTicketIDs(t, "WG-")

	qt, err := g.Enqueue(Request{RunID: "r1", WorkerID: 1})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	st := readStateForTest(t, root)
	assertTicketCount(t, st, 1)
	seqBefore := st.NextSequence

	// Wait and grant never allocates a second ticket or changes NextSequence.
	lease, err := qt.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if lease == nil {
		t.Fatal("Wait returned nil lease")
	}

	st = readStateForTest(t, root)
	assertTicketCount(t, st, 1)
	assertNextSequence(t, st, seqBefore)

	if err := lease.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	assertNoTickets(t, root)
}

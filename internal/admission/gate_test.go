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

func TestGateAcquireAlreadyCancelled(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	before := readStateForTest(t, root)

	_, err := g.Acquire(ctx, Request{RunID: "r1", WorkerID: 1})
	if err == nil {
		t.Fatal("Acquire(cancelled) = nil, want error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	after := readStateForTest(t, root)
	if len(after.Tickets) != len(before.Tickets) {
		t.Fatalf("state changed: before=%d tickets, after=%d", len(before.Tickets), len(after.Tickets))
	}
	if after.NextSequence != before.NextSequence {
		t.Fatalf("NextSequence changed: %d → %d", before.NextSequence, after.NextSequence)
	}
}

func TestGateAcquireAndRelease(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 1)
	nIDs := installSequentialTicketIDs(t, "T-")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lease, err := g.Acquire(ctx, Request{RunID: "run-42", WorkerID: 7})
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

	lease1, err := g.Acquire(ctx, Request{RunID: "r1", WorkerID: 1})
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
		l, e := g.Acquire(ctx, Request{RunID: "r2", WorkerID: 2})
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
	g, err := Open(root, 1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	lease0, err := g.Acquire(ctx, Request{RunID: "r0", WorkerID: 1})
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

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

	// Start A only, then poll until durably queued before starting B.
	go func() {
		defer wg.Done()
		l, e := g.Acquire(ctx, Request{RunID: "rA", WorkerID: 10})
		results <- result{"A", l, e}
	}()
	// Poll until rA is queued.
	deadline := time.Now().Add(2 * time.Second)
	for {
		st := readStateForTest(t, root)
		for _, tk := range st.Tickets {
			if tk.RunID == "rA" && tk.State == ticketQueued {
				goto aQueued
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("rA did not become queued within timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
aQueued:

	// Start B now.
	go func() {
		defer wg.Done()
		l, e := g.Acquire(ctx, Request{RunID: "rB", WorkerID: 20})
		results <- result{"B", l, e}
	}()

	// Poll until rB is queued and capture sequences.
	seqMismatch := false
	deadline = time.Now().Add(2 * time.Second)
	for {
		st := readStateForTest(t, root)
		aSeq, bSeq := -1, -1
		for _, tk := range st.Tickets {
			if tk.RunID == "rA" && tk.State == ticketQueued {
				aSeq = tk.Sequence
			}
			if tk.RunID == "rB" && tk.State == ticketQueued {
				bSeq = tk.Sequence
			}
		}
		if aSeq >= 0 && bSeq >= 0 {
			if aSeq >= bSeq {
				seqMismatch = true
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("rA or rB did not become queued within timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Release held lease → earlier queued (A) must be granted first.
	if err := lease0.Release(); err != nil {
		t.Fatalf("Release: %v", err)
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
			t.Fatal("timed out waiting for both Acquire results")
		}
	}

	// Both goroutines have returned; assert ordering.
	if seqMismatch {
		t.Fatalf("A sequence must be < B sequence")
	}
	if rA.err != nil {
		t.Fatalf("A Acquire error: %v", rA.err)
	}
	if rB.err != nil {
		t.Fatalf("B Acquire error: %v", rB.err)
	}
	if rA.lease == nil {
		t.Fatal("A Acquire returned nil lease")
	}
	if rB.lease == nil {
		t.Fatal("B Acquire returned nil lease")
	}
	if len(order) != 2 || order[0] != "A" || order[1] != "B" {
		t.Fatalf("unexpected arrival order: %v, want [A B]", order)
	}
}

func TestGateCancelQueuedAcquire(t *testing.T) {
	root := t.TempDir()
	g, err := Open(root, 1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r1Lease, err := g.Acquire(ctx, Request{RunID: "r1", WorkerID: 1})
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	type acqResult struct {
		lease *Lease
		err   error
	}
	bDone := make(chan acqResult, 1)
	go func() {
		l, e := g.Acquire(ctx, Request{RunID: "r2", WorkerID: 2})
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

	lease, err := g.Acquire(context.Background(), Request{RunID: "new-run", WorkerID: 2})
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

	lease, err := g.Acquire(context.Background(), Request{RunID: "new-run", WorkerID: 2})
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
		_, e := g.Acquire(ctx, Request{RunID: "new-run", WorkerID: 2})
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
		_, e := g.Acquire(ctx, Request{RunID: "run-new", WorkerID: 2})
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
		_, e := g.Acquire(ctx, Request{RunID: "run-new", WorkerID: 2})
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

	_, err := g.Acquire(context.Background(), Request{RunID: "r1", WorkerID: 1})
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
	lease, err := g.Acquire(ctx, Request{RunID: "r1", WorkerID: 1})
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

	lease, err := g.Acquire(ctx, Request{RunID: "r1", WorkerID: 1})
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	_ = lease

	// Second Acquire fails: state.json already has "dup-id" → duplicate
	// ID rejected by saveState via validateState.
	_, err = g.Acquire(ctx, Request{RunID: "r2", WorkerID: 2})
	if err == nil {
		t.Fatal("second Acquire(dup) = nil, want error")
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

	_, err = g.Acquire(context.Background(), Request{RunID: "r1", WorkerID: 1})
	if err == nil {
		t.Fatal("Acquire(exhausted) = nil, want error")
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

	lease, err := g.Acquire(ctx, Request{RunID: "r1", WorkerID: 1})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	bDone := make(chan error, 1)
	go func() {
		_, e := g.Acquire(ctx, Request{RunID: "r2", WorkerID: 2})
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
		_, e := g.Acquire(ctx, Request{RunID: "rA", WorkerID: 10})
		aDone <- e
	}()
	go func() {
		_, e := g.Acquire(ctx, Request{RunID: "rB", WorkerID: 20})
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
	l1, err := g.Acquire(ctx, Request{RunID: "r1", WorkerID: 1})
	if err != nil {
		t.Fatalf("Acquire 1: %v", err)
	}
	l2, err := g.Acquire(ctx, Request{RunID: "r2", WorkerID: 2})
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

	lease, err := g.Acquire(context.Background(), Request{RunID: "r1", WorkerID: 1})
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

func TestGateAcquireRunIDEmpty(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 1)

	_, err := g.Acquire(context.Background(), Request{RunID: "", WorkerID: 1})
	if err == nil {
		t.Fatal("Acquire(empty RunID) = nil, want error")
	}
}

func TestGateAcquireWorkerIDNotPositive(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 1)

	for _, wid := range []int{0, -1} {
		_, err := g.Acquire(context.Background(), Request{RunID: "r1", WorkerID: wid})
		if err == nil {
			t.Errorf("Acquire(WorkerID=%d) = nil, want error", wid)
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
	lease, err := g.Acquire(context.Background(), Request{RunID: "retry-run", WorkerID: 1})
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

//go:build darwin || linux

package admission

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReconcileRemovesStaleRetainsLiveAndUncertainOrdered verifies that
// Reconcile removes only stale queued and leased tickets, retains every
// live (ownerSame) and uncertain/unreadable (ownerUncertain) ticket in
// original order, and never changes NextSequence.
func TestReconcileRemovesStaleRetainsLiveAndUncertainOrdered(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 4)

	// live: gate owner → ownerSame → not reaped.
	liveQueued := ticket{
		ID: "live-q", Sequence: 1, RunID: "live-q-run",
		WorkerID: 1, OwnerPID: g.owner.PID, OwnerCreateTime: g.owner.CreateTime,
		State: ticketQueued,
	}
	liveLeased := ticket{
		ID: "live-l", Sequence: 2, RunID: "live-l-run",
		WorkerID: 2, OwnerPID: g.owner.PID, OwnerCreateTime: g.owner.CreateTime,
		State: ticketLeased,
	}
	// stale: absent PID → ownerStale → reaped.
	staleQueued := ticket{
		ID: "stale-q", Sequence: 3, RunID: "stale-q-run",
		WorkerID: 3, OwnerPID: 9999, OwnerCreateTime: 9999000,
		State: ticketQueued,
	}
	staleLeased := ticket{
		ID: "stale-l", Sequence: 4, RunID: "stale-l-run",
		WorkerID: 4, OwnerPID: 8888, OwnerCreateTime: 8888000,
		State: ticketLeased,
	}
	// uncertain: pidCreateTime errors → ownerUncertain → not reaped.
	uncertainPID := 7777
	uncertain := ticket{
		ID: "unc-q", Sequence: 5, RunID: "unc-q-run",
		WorkerID: 5, OwnerPID: uncertainPID, OwnerCreateTime: 7777000,
		State: ticketQueued,
	}

	// Reconfigure seams so uncertain PID lookup returns an error.
	origPidExists := pidExists
	origPidCreateTime := pidCreateTime
	t.Cleanup(func() {
		pidExists = origPidExists
		pidCreateTime = origPidCreateTime
	})
	pidExists = func(pid int) (bool, error) {
		if pid == g.owner.PID {
			return true, nil
		}
		if pid == uncertainPID {
			return false, errors.New("perm denied")
		}
		return false, nil
	}
	pidCreateTime = func(pid int) (int64, error) {
		if pid == g.owner.PID {
			return g.owner.CreateTime, nil
		}
		return 0, errors.New("perm denied")
	}

	if err := saveState(root, state{
		SchemaVersion: 1, NextSequence: 6,
		Tickets: []ticket{liveQueued, liveLeased, staleQueued, staleLeased, uncertain},
	}); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	if err := g.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	st := readStateForTest(t, root)
	assertNextSequence(t, st, 6)
	// Retained: liveQueued, liveLeased, uncertain — in original order.
	assertTicketCount(t, st, 3)
	if st.Tickets[0].ID != "live-q" {
		t.Errorf("ticket[0] ID = %q, want live-q", st.Tickets[0].ID)
	}
	if st.Tickets[0].State != ticketQueued {
		t.Errorf("ticket[0] State = %q, want queued", st.Tickets[0].State)
	}
	if st.Tickets[1].ID != "live-l" {
		t.Errorf("ticket[1] ID = %q, want live-l", st.Tickets[1].ID)
	}
	if st.Tickets[1].State != ticketLeased {
		t.Errorf("ticket[1] State = %q, want leased", st.Tickets[1].State)
	}
	if st.Tickets[2].ID != "unc-q" {
		t.Errorf("ticket[2] ID = %q, want unc-q", st.Tickets[2].ID)
	}
	if st.Tickets[2].State != ticketQueued {
		t.Errorf("ticket[2] State = %q, want queued", st.Tickets[2].State)
	}
}

// TestReconcileDoesNotRewriteStateFileIdentity verifies that a clean
// Reconcile (no stale entries to reap) does not rewrite the state file.
// os.SameFile compares the file identity (inode on Unix) before and
// after; an atomic rename mutation would change it.
func TestReconcileDoesNotRewriteStateFileIdentity(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 1)

	// Write a clean state with one live ticket and no stale entries.
	live := ticket{
		ID: "live-1", Sequence: 1, RunID: "live-run",
		WorkerID: 1, OwnerPID: g.owner.PID, OwnerCreateTime: g.owner.CreateTime,
		State: ticketQueued,
	}
	if err := saveState(root, state{
		SchemaVersion: 1, NextSequence: 2,
		Tickets: []ticket{live},
	}); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	// Capture file identity before Reconcile.
	sp := statePath(root)
	infoBefore, err := os.Stat(sp)
	if err != nil {
		t.Fatalf("Stat before: %v", err)
	}

	if err := g.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// File identity must be unchanged.
	infoAfter, err := os.Stat(sp)
	if err != nil {
		t.Fatalf("Stat after: %v", err)
	}
	if !os.SameFile(infoBefore, infoAfter) {
		t.Fatalf("state file identity changed (atomic rename mutation detected): before=%+v, after=%+v", infoBefore, infoAfter)
	}

	// Content is unchanged.
	st := readStateForTest(t, root)
	assertTicketCount(t, st, 1)
	assertNextSequence(t, st, 2)
	if st.Tickets[0].ID != "live-1" {
		t.Errorf("ticket ID = %q, want live-1", st.Tickets[0].ID)
	}
}

// TestReconcileCorruptStateReturnsWrappedErrorNoMutation verifies that
// corrupt state returns a wrapped reconcile error without modifying the
// file on disk.
func TestReconcileCorruptStateReturnsWrappedErrorNoMutation(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 1)

	raw := `{bad json`
	writeRawState(t, root, raw)

	err := g.Reconcile()
	if err == nil {
		t.Fatal("Reconcile(corrupt) = nil, want error")
	}
	// The load error surfaces with its own context and a decode signal,
	// wrapped by the reconcile prefix.
	got := err.Error()
	if !strings.Contains(got, "admission reconcile:") {
		t.Errorf("error %q missing reconcile prefix", got)
	}
	if !strings.Contains(got, "load:") {
		t.Errorf("error %q missing load context", got)
	}
	if !strings.Contains(got, "invalid character") && !strings.Contains(got, "unexpected") && !strings.Contains(got, "json") {
		t.Errorf("error %q missing JSON/decode signal", got)
	}

	// File on disk must be unchanged.
	gotRaw, rerr := os.ReadFile(filepath.Join(root, "state.json"))
	if rerr != nil {
		t.Fatalf("ReadFile: %v", rerr)
	}
	if string(gotRaw) != raw {
		t.Fatalf("state.json was modified: before=%q, after=%q", raw, string(gotRaw))
	}
}

// TestReconcileSymlinkStateReturnsWrappedErrorNoMutation verifies that
// a symlinked state file returns a wrapped reconcile error without
// modifying the symlink or its target.
func TestReconcileSymlinkStateReturnsWrappedErrorNoMutation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping symlink test in short mode")
	}
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 1)

	target := filepath.Join(t.TempDir(), "target")
	validState := `{"schemaVersion":1,"nextSequence":1,"tickets":[]}`
	if err := os.WriteFile(target, []byte(validState), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(root, "state.json")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	err := g.Reconcile()
	if err == nil {
		t.Fatal("Reconcile(symlink) = nil, want error")
	}
	// The symlink refusal must be wrapped by the reconcile prefix.
	got := err.Error()
	if !strings.Contains(got, "admission reconcile:") {
		t.Errorf("error %q missing reconcile prefix", got)
	}
	if !strings.Contains(got, "load:") {
		t.Errorf("error %q missing load context", got)
	}

	// Symlink is untouched.
	fi, lerr := os.Lstat(filepath.Join(root, "state.json"))
	if lerr != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink changed: %v %v", lerr, fi)
	}
	// Target is untouched.
	gotTarget, rerr := os.ReadFile(target)
	if rerr != nil {
		t.Fatalf("ReadFile: %v", rerr)
	}
	if string(gotTarget) != validState {
		t.Fatalf("target changed: before=%q, after=%q", validState, string(gotTarget))
	}
}

// TestReconcileCleanLeavesZeroTicketsAndNextSequenceWithoutProbe verifies
// that Reconcile on a live owner's clean state performs no probe: it never
// enqueues a ticket and never advances NextSequence.
func TestReconcileCleanLeavesZeroTicketsAndNextSequenceWithoutProbe(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 1)

	// Empty, valid state: nextSequence 1, no tickets.
	st := readStateForTest(t, root)
	assertNextSequence(t, st, 1)
	assertTicketCount(t, st, 0)

	if err := g.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	st = readStateForTest(t, root)
	assertTicketCount(t, st, 0)
	assertNextSequence(t, st, 1)
}

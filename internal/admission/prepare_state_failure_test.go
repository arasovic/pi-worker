//go:build darwin || linux

package admission

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPrepareSequenceOverflow verifies that when NextSequence is near
// math.MaxInt and a multi-request batch cannot advance without wrapping,
// Prepare consumes two generated IDs, detects overflow before appending any
// ticket, returns nil handles, and leaves state unchanged.
func TestPrepareSequenceOverflow(t *testing.T) {
	root := t.TempDir()
	restoreGateSeams(t, 5000, 5000000, map[int]pidResp{
		5000: {exists: true, createTime: 5000000},
	})
	nIDs := installSequentialTicketIDs(t, "PO-")

	st := state{
		SchemaVersion: 1,
		NextSequence:  math.MaxInt - 1, // leaving room for zero but not a two-request batch
		Tickets:       []ticket{},
	}
	if err := saveState(root, st); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	g, err := Open(root, 5)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	savedBytesBefore, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatalf("ReadFile before: %v", err)
	}

	handles, err := g.Prepare([]Request{
		{RunID: "r", WorkerID: 1},
		{RunID: "r", WorkerID: 2},
	})
	if handles != nil {
		t.Errorf("handles=%v, want nil", handles)
	}
	if err == nil {
		t.Fatal("Prepare(overflow) = nil, want error")
	}
	const prefix = "admission prepare:"
	if !strings.HasPrefix(err.Error(), prefix) {
		t.Errorf("error=%q missing '%s' prefix", err, prefix)
	}
	if !strings.Contains(err.Error(), "overflow") {
		t.Errorf("error=%q should mention overflow", err)
	}

	// Exact state must be unchanged.
	stAfter := readStateForTest(t, root)
	if stAfter.SchemaVersion != 1 || stAfter.NextSequence != math.MaxInt-1 || len(stAfter.Tickets) != 0 {
		t.Fatalf("state changed: schema=%d nextSeq=%d tickets=%d", stAfter.SchemaVersion, stAfter.NextSequence, len(stAfter.Tickets))
	}
	savedBytesAfter, _ := os.ReadFile(filepath.Join(root, "state.json"))
	if string(savedBytesBefore) != string(savedBytesAfter) {
		t.Fatal("state.json was modified by failed Prepare")
	}
	if c := nIDs(); c != 2 {
		t.Errorf("ticket IDs consumed = %d, want 2", c)
	}
}

// TestPrepareCorruptStateAfterGateOpen ensures that after a successful
// Gate.Open, replacing state.json with corrupt bytes causes Prepare to
// return nil handles, a prefixed error, and leave the corrupt bytes
// exactly as written — never rolled back or cleaned up.
func TestPrepareCorruptStateAfterGateOpen(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 5)
	installSequentialTicketIDs(t, "CS-")

	// Save an explicit empty state so that Gate.Open (which doesn't create
	// state.json on an empty root) is covered; verify save succeeds.
	if err := saveState(root, emptyState()); err != nil {
		t.Fatalf("saveState(empty): %v", err)
	}

	// Read existing state.json to confirm it's valid before corrupting.
	if _, err := os.ReadFile(filepath.Join(root, "state.json")); err != nil {
		t.Fatalf("read state: %v", err)
	}

	// Corrupt state.json after Gate.Open.
	corruptBytes := []byte(`{schemaVersion:broken}`)
	if err := os.WriteFile(filepath.Join(root, "state.json"), corruptBytes, 0o600); err != nil {
		t.Fatalf("WriteFile corrupt: %v", err)
	}

	handles, err := g.Prepare([]Request{{RunID: "r", WorkerID: 1}})
	if handles != nil {
		t.Errorf("handles=%v, want nil", handles)
	}
	if err == nil {
		t.Fatal("Prepare(corrupt) = nil, want error")
	}
	const prefix = "admission prepare:"
	if !strings.HasPrefix(err.Error(), prefix) {
		t.Errorf("error=%q missing '%s' prefix", err, prefix)
	}

	// Corrupt bytes must remain untouched on disk.
	after, rerr := os.ReadFile(filepath.Join(root, "state.json"))
	if rerr != nil {
		t.Fatalf("ReadFile: %v", rerr)
	}
	if string(after) != string(corruptBytes) {
		t.Fatalf("state.json was replaced:\nbefore: %q\nafter:  %q", corruptBytes, after)
	}
	assertNoTempFiles(t, root)
}

// TestPrepareSymlinkStateAfterGateOpen confirms that after Gate.Open,
// swapping state.json into a symlink pointing at an outside target
// causes Prepare to return nil handles and a prefixed error, while
// the link stays intact and its target bytes are unchanged.
// Skipped only if os.Symlink itself is demonstrably unsupported.
func TestPrepareSymlinkStateAfterGateOpen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks is not reliably available on Windows")
	}

	root := t.TempDir()
	g, _ := openGateForTest(t, root, 5)
	installSequentialTicketIDs(t, "SS-")

	// Ensure state.json exists so that the subsequent removal+symlink test
	// scenario is realistic; Gate.Open on an empty root does not create it.
	if err := saveState(root, emptyState()); err != nil {
		t.Fatalf("saveState(empty): %v", err)
	}

	// Outside target living alongside root so it survives t.TempDir cleanup.
	targetPath := filepath.Join(t.TempDir(), "outside-state.json")
	outsideContent := `{"schemaVersion":1,"nextSequence":1,"tickets":[{"id":"tgt","sequence":1,"runId":"t-run","workerId":1,"ownerPid":1234,"ownerCreateTime":1234000,"state":"queued"}]}` + "\n"
	if err := os.WriteFile(targetPath, []byte(outsideContent), 0o600); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}

	// Remove the real state.json and replace it with a symlink.
	statePath := filepath.Join(root, "state.json")
	if err := os.Remove(statePath); err != nil {
		t.Fatalf("Remove existing state: %v", err)
	}
	if err := os.Symlink(targetPath, statePath); err != nil {
		t.Skipf("os.Symlink failed: %v", err)
	}

	info, lerr := os.Lstat(statePath)
	if lerr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("state.json is no longer a symlink: %v %v", lerr, info.Mode())
	}

	handles, err := g.Prepare([]Request{{RunID: "r", WorkerID: 1}})
	if handles != nil {
		t.Errorf("handles=%v, want nil", handles)
	}
	if err == nil {
		t.Fatal("Prepare(symlink) = nil, want error")
	}
	const prefix = "admission prepare:"
	if !strings.HasPrefix(err.Error(), prefix) {
		t.Errorf("error=%q missing '%s' prefix", err, prefix)
	}

	// Symlink must remain a symlink pointing at the same target.
	info, lerr = os.Lstat(statePath)
	if lerr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link removed or replaced: %v %v", lerr, info.Mode())
	}
	if got, _ := os.Readlink(statePath); got != targetPath {
		t.Errorf("link target changed to %q, want %q", got, targetPath)
	}

	// Target bytes must be unchanged.
	targetAfter, rerr := os.ReadFile(targetPath)
	if rerr != nil {
		t.Fatalf("ReadFile target: %v", rerr)
	}
	if string(targetAfter) != outsideContent {
		t.Fatalf("target modified:\nbefore: %q\nafter:  %q", outsideContent, targetAfter)
	}
	assertNoTempFiles(t, root)
}

// TestPrepareSaveFailure replaces saveGateState with a function that
// returns a sentinel error without writing, opens a Gate, calls one-
// request Prepare, and requires nil handles, a prefixed error containing
// the sentinel, exactly one generated ID consumed, and exact prior state
// bytes/NextSequence/tickets unchanged.
func TestPrepareSaveFailure(t *testing.T) {
	root := t.TempDir()
	owner := restoreGateSeams(t, 5000, 5000000, map[int]pidResp{
		5000: {exists: true, createTime: 5000000},
	})
	nIDs := installSequentialTicketIDs(t, "PSF-")

	// Build deterministic state and persist it with the real saveState.
	wantSt := state{
		SchemaVersion: 1,
		NextSequence:  2,
		Tickets: []ticket{{
			ID: "prefetch-1", Sequence: 1, RunID: "x", WorkerID: 1,
			OwnerPID: owner.PID, OwnerCreateTime: owner.CreateTime, State: ticketQueued,
		}},
	}
	if err := saveState(root, wantSt); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	exactBytes, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatalf("ReadFile saved state: %v", err)
	}

	// Open the Gate normally.
	g, err := Open(root, 5)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Swap saveGateState to a failing stub and restore on cleanup.
	savedSaveGateState := saveGateState
	saveGateState = func(_ string, _ state) error {
		return errors.New("save fail")
	}
	t.Cleanup(func() { saveGateState = savedSaveGateState })

	handles, err := g.Prepare([]Request{{RunID: "r", WorkerID: 2}})
	if handles != nil {
		t.Errorf("handles=%v, want nil", handles)
	}
	if err == nil {
		t.Fatal("Prepare(save fail) = nil, want error")
	}
	if !strings.HasPrefix(err.Error(), "admission prepare:") {
		t.Errorf("error=%q missing 'admission prepare:' prefix", err)
	}
	if !strings.Contains(err.Error(), "save fail") {
		t.Errorf("error=%q missing 'save fail'", err)
	}
	if c := nIDs(); c != 1 {
		t.Errorf("ticket IDs consumed = %d, want 1", c)
	}
	st := readStateForTest(t, root)
	if st.SchemaVersion != 1 || st.NextSequence != 2 || len(st.Tickets) != 1 {
		t.Fatalf("state changed unexpectedly: schema=%d nextSeq=%d tickets=%d", st.SchemaVersion, st.NextSequence, len(st.Tickets))
	}
	gotBytes, rerr := os.ReadFile(filepath.Join(root, "state.json"))
	if rerr != nil {
		t.Fatalf("ReadFile after: %v", rerr)
	}
	if string(gotBytes) != string(exactBytes) {
		t.Fatalf("state.json changed:\nwant: %q\ngot:  %q", exactBytes, gotBytes)
	}
}

// TestPrepareCollisionStillPersistsStaleReaping verifies the documented
// exception: when Prepare generates a ticket ID that collides with a
// retained ticket, it aborts the entire batch without persisting any new
// ticket, but updateState has already committed stale-owner reaping.
//
// Current-owner PID 5000 is live; stale ticket owned by absent PID 9999.
// State contains sequence 1 (stale, PID 9999) and sequence 2 (retained,
// owned by PID 5000, ID "collision"). Forcing newTicketID to return
// "collision" triggers the duplicate-ID error, which leaves the stale
// ticket reaped, the collision ticket untouched at sequence 2, and
// NextSequence still at 3.
func TestPrepareCollisionStillPersistsStaleReaping(t *testing.T) {
	root := t.TempDir()
	owner := restoreGateSeams(t, 5000, 5000000, map[int]pidResp{
		5000: {exists: true, createTime: 5000000},
	})

	// Track how many new-ticket-ID calls were made.
	var consumedIDs int
	origNewTicketID := newTicketID
	t.Cleanup(func() { newTicketID = origNewTicketID })
	newTicketID = func() (string, error) {
		consumedIDs++
		return "collision", nil
	}

	// Preload: stale ticket (seq 1, absent PID 9999) and retained ticket
	// (seq 2, same owner, ID "collision") — both queued.
	stale := ticket{
		ID: "old-stale", Sequence: 1, RunID: "old-run",
		WorkerID: 1, OwnerPID: 9999, OwnerCreateTime: 9999000,
		State: ticketQueued,
	}
	retained := ticket{
		ID: "collision", Sequence: 2, RunID: "retained-run",
		WorkerID: 2, OwnerPID: owner.PID, OwnerCreateTime: owner.CreateTime,
		State: ticketQueued,
	}
	if err := saveState(root, state{
		SchemaVersion: 1, NextSequence: 3,
		Tickets: []ticket{stale, retained},
	}); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	g, err := Open(root, 5)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// One-request Prepare: newTicketID returns "collision", which collides
	// with the retained ticket at seq 2.  Prepare must fail closed.
	handles, err := g.Prepare([]Request{{RunID: "r", WorkerID: 2}})
	if handles != nil {
		t.Errorf("handles=%v, want nil", handles)
	}
	if err == nil {
		t.Fatal("Prepare(collision) = nil, want error")
	}
	const prefix = "admission prepare:"
	if !strings.HasPrefix(err.Error(), prefix) {
		t.Errorf("error=%q missing '%s' prefix", err, prefix)
	}
	if !strings.Contains(err.Error(), "collision") {
		t.Errorf("error should mention collision, got: %v", err)
	}

	// Durable state: stale ticket reaped, no new batch ticket persisted,
	// retained collision ticket unchanged at sequence 2, NextSequence=3.
	st := readStateForTest(t, root)
	assertNextSequence(t, st, 3)
	assertTicketCount(t, st, 1)
	if st.Tickets[0].ID != "collision" {
		t.Errorf("remaining ticket ID = %q, want collision", st.Tickets[0].ID)
	}
	if st.Tickets[0].Sequence != 2 {
		t.Errorf("remaining ticket sequence = %d, want 2", st.Tickets[0].Sequence)
	}

	// Exactly one ticket ID was consumed (before the failing updateState).
	if consumedIDs != 1 {
		t.Errorf("ticket IDs consumed = %d, want 1", consumedIDs)
	}
}

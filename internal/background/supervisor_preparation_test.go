//go:build darwin || linux

package background

import (
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/arasovic/pi-worker/internal/admission"
)

// diskAdmissionState is the minimal wire shape of admission state.json,
// decoded locally so the tests can assert durable admission state without
// reaching into admission internals or duplicating its validation.
type diskAdmissionState struct {
	SchemaVersion int                   `json:"schemaVersion"`
	NextSequence  int                   `json:"nextSequence"`
	Tickets       []diskAdmissionTicket `json:"tickets"`
}

// diskAdmissionTicket is one ticket entry inside admission state.json.
type diskAdmissionTicket struct {
	ID              string `json:"id"`
	Sequence        int    `json:"sequence"`
	RunID           string `json:"runId"`
	WorkerID        int    `json:"workerId"`
	OwnerPID        int    `json:"ownerPid"`
	OwnerCreateTime int64  `json:"ownerCreateTime"`
	State           string `json:"state"`
}

// readAdmissionState decodes the durable admission document at
// <root>/state.json. The file must exist. The decoder rejects unknown
// fields and requires exactly one JSON document, mirroring the strict
// decode admission itself performs on the same document.
func readAdmissionState(t *testing.T, admissionRoot string) diskAdmissionState {
	t.Helper()
	f, err := os.Open(filepath.Join(admissionRoot, "state.json"))
	if err != nil {
		t.Fatalf("read admission state.json: %v", err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var st diskAdmissionState
	if err := dec.Decode(&st); err != nil {
		t.Fatalf("decode admission state.json: %v", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		t.Fatalf("admission state.json must carry exactly one JSON document, got extra content: %v", err)
	}
	return st
}

// seedAdmissionState writes st as the admission state document at
// <admissionRoot>/state.json before any Gate opens that root.
func seedAdmissionState(t *testing.T, admissionRoot string, st diskAdmissionState) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(admissionRoot, "state.json"), mustMarshalJSON(t, st), 0o600); err != nil {
		t.Fatalf("seed admission state.json: %v", err)
	}
}

// dirEntryNames returns the direct children of dir, failing the test when
// the directory cannot be read.
func dirEntryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}

// requireDirEmpty fails the test when dir contains any entry.
func requireDirEmpty(t *testing.T, dir string) {
	t.Helper()
	if names := dirEntryNames(t, dir); len(names) != 0 {
		t.Fatalf("dir %s is not empty: %v", dir, names)
	}
}

// normalizeEmptyWorkerSlices returns a copy of snap with every empty
// per-worker Writes and Data slice set to nil. The snapshot wire format
// uses omitempty, so an empty non-nil Data slice built by the projection
// (a task without material) and a nil one serialize identically and decode
// as nil; normalizing both sides to the decoded shape keeps the reload
// comparison at the fidelity the wire can actually express.
func normalizeEmptyWorkerSlices(snap Snapshot) Snapshot {
	for i := range snap.Workers {
		if len(snap.Workers[i].Task.Writes) == 0 {
			snap.Workers[i].Task.Writes = nil
		}
		if len(snap.Workers[i].Task.Data) == 0 {
			snap.Workers[i].Task.Data = nil
		}
	}
	return snap
}

// newStartRequestWithTempRoots returns a valid start request whose
// backgroundRoot and admissionRoot are fresh temporary directories, plus
// those roots for direct assertion.
func newStartRequestWithTempRoots(t *testing.T) (req supervisorStartRequest, backgroundRoot, admissionRoot string) {
	t.Helper()
	req = validStartRequest()
	req.backgroundRoot = t.TempDir()
	req.admissionRoot = t.TempDir()
	return req, req.backgroundRoot, req.admissionRoot
}

// TestPrepareSupervisorStartSuccess runs the full happy path with two
// ordered tasks. It requires that the accepted Snapshot is durable and
// reloads from disk unchanged; that the snapshot supervisor identity is
// positive and equals the stored owner identity of a newly opened
// admission Gate over the same root; and that state.json carries exactly
// one queued durable ticket per worker, in task order, under that same
// owner identity.
func TestPrepareSupervisorStartSuccess(t *testing.T) {
	req, backgroundRoot, admissionRoot := newStartRequestWithTempRoots(t)
	if len(req.tasks) != 2 {
		t.Fatalf("test setup: validStartRequest must carry two tasks, got %d", len(req.tasks))
	}

	// Seed the admission sequence space ahead of the worker IDs so the
	// durable ticket sequences cannot accidentally coincide with worker
	// order: tickets must carry sequences from the gate's own counter
	// (100 and 101 here), not worker IDs 1 and 2.
	seedAdmissionState(t, admissionRoot, diskAdmissionState{
		SchemaVersion: 1,
		NextSequence:  100,
		Tickets:       []diskAdmissionTicket{},
	})

	prep, err := prepareSupervisorStart(req)
	if err != nil {
		t.Fatalf("prepareSupervisorStart: %v", err)
	}
	if prep == nil {
		t.Fatal("prepareSupervisorStart returned a nil preparation")
	}

	// The accepted Snapshot sits on disk at <root>/<runId>/snapshot.json
	// and reloads from disk exactly as built.
	snapPath := filepath.Join(backgroundRoot, req.runID, "snapshot.json")
	if _, err := os.Stat(snapPath); err != nil {
		t.Fatalf("snapshot on disk: %v", err)
	}
	loaded, err := prep.store.Load(req.runID)
	if err != nil {
		t.Fatalf("reload accepted snapshot from disk: %v", err)
	}
	if !reflect.DeepEqual(normalizeEmptyWorkerSlices(loaded), normalizeEmptyWorkerSlices(prep.snapshot)) {
		t.Fatalf("reloaded snapshot differs from prepared one:\n got: %+v\nwant: %+v", loaded, prep.snapshot)
	}

	// A newly opened Gate over the same admission root exposes the stored
	// owner identity: positive, and equal to the snapshot supervisor.
	gate, err := admission.Open(admissionRoot, req.maxModelWorkers)
	if err != nil {
		t.Fatalf("open admission gate for assertion: %v", err)
	}
	owner := gate.OwnerIdentity()
	if owner.PID <= 0 || owner.CreateTime <= 0 {
		t.Fatalf("gate owner identity must be positive, got pid=%d createTime=%d", owner.PID, owner.CreateTime)
	}
	wantSupervisor := ProcessIdentity{PID: owner.PID, CreateTime: owner.CreateTime}
	if prep.snapshot.Supervisor != wantSupervisor {
		t.Fatalf("snapshot supervisor %+v != gate owner identity %+v", prep.snapshot.Supervisor, owner)
	}

	// One queued durable ticket per worker in task order — ticket i
	// belongs to worker ID i+1 — all under the same owner identity.
	st := readAdmissionState(t, admissionRoot)
	if len(st.Tickets) != len(req.tasks) {
		t.Fatalf("durable tickets = %d, want %d: %+v", len(st.Tickets), len(req.tasks), st.Tickets)
	}
	for i, tk := range st.Tickets {
		wantWorker := i + 1
		if tk.WorkerID != wantWorker {
			t.Errorf("ticket[%d].WorkerID = %d, want %d", i, tk.WorkerID, wantWorker)
		}
		if tk.RunID != req.runID {
			t.Errorf("ticket[%d].RunID = %q, want %q", i, tk.RunID, req.runID)
		}
		if i > 0 && tk.Sequence <= st.Tickets[i-1].Sequence {
			t.Errorf("ticket[%d].Sequence = %d, want strictly increasing after ticket[%d] sequence %d", i, tk.Sequence, i-1, st.Tickets[i-1].Sequence)
		}
		if tk.Sequence == wantWorker {
			t.Errorf("ticket[%d].Sequence = %d equals worker ID %d; durable sequences must come from the gate's own sequence space, not worker order", i, tk.Sequence, wantWorker)
		}
		if tk.State != "queued" {
			t.Errorf("ticket[%d].State = %q, want %q", i, tk.State, "queued")
		}
		if tk.OwnerPID != owner.PID || tk.OwnerCreateTime != owner.CreateTime {
			t.Errorf("ticket[%d] owner (%d/%d) != gate owner (%d/%d)", i, tk.OwnerPID, tk.OwnerCreateTime, owner.PID, owner.CreateTime)
		}
	}
}

// TestPrepareSupervisorStartInvalidRequestCreatesNoState verifies that a
// request failing phase-1 validation never touches either state root: no
// snapshot store content and no admission state may appear.
func TestPrepareSupervisorStartInvalidRequestCreatesNoState(t *testing.T) {
	req, backgroundRoot, admissionRoot := newStartRequestWithTempRoots(t)
	req.runID = "bogus"

	prep, err := prepareSupervisorStart(req)
	if err == nil {
		t.Fatal("prepareSupervisorStart accepted an invalid request")
	}
	if !strings.Contains(err.Error(), "validate request") {
		t.Fatalf("error %q does not mention request validation", err)
	}
	if prep != nil {
		t.Fatalf("failed prepare returned non-nil preparation %+v", prep)
	}

	requireDirEmpty(t, backgroundRoot)
	requireDirEmpty(t, admissionRoot)
}

// TestPrepareSupervisorStartGateOpenFailureCreatesNoSnapshot verifies that
// when the admission Gate cannot open over a corrupt state.json, the
// preparation fails before persisting anything: the background root gains
// no run directory, and the corrupt admission document is left untouched.
func TestPrepareSupervisorStartGateOpenFailureCreatesNoSnapshot(t *testing.T) {
	req, backgroundRoot, admissionRoot := newStartRequestWithTempRoots(t)

	// A corrupt state.json makes admission.Open fail closed.
	statePath := filepath.Join(admissionRoot, "state.json")
	corrupt := []byte(`{"schemaVersion":`)
	if err := os.WriteFile(statePath, corrupt, 0o600); err != nil {
		t.Fatalf("write corrupt state.json: %v", err)
	}

	prep, err := prepareSupervisorStart(req)
	if err == nil {
		t.Fatal("prepareSupervisorStart succeeded over a corrupt admission state")
	}
	if !strings.Contains(err.Error(), "open admission gate") {
		t.Fatalf("error %q does not mention the admission gate", err)
	}
	if prep != nil {
		t.Fatalf("failed prepare returned non-nil preparation %+v", prep)
	}

	// No snapshot state root was created.
	requireDirEmpty(t, backgroundRoot)

	// The corrupt document remains exactly as written.
	after, rerr := os.ReadFile(statePath)
	if rerr != nil {
		t.Fatalf("read corrupt state.json after failure: %v", rerr)
	}
	if string(after) != string(corrupt) {
		t.Fatalf("state.json was modified by failed prepare:\nbefore: %q\nafter:  %q", corrupt, after)
	}
}

// TestPrepareSupervisorStartAtomicPrepareFailureCreatesNothing seeds a
// valid admission state whose nextSequence sits at math.MaxInt, so the
// two-task batch cannot advance without wrapping and the single atomic
// Gate.Prepare transition fails. The preparation must fail before phase 7:
// no Snapshot is created and no ticket is added, leaving the seeded
// state.json bytes untouched.
func TestPrepareSupervisorStartAtomicPrepareFailureCreatesNothing(t *testing.T) {
	req, backgroundRoot, admissionRoot := newStartRequestWithTempRoots(t)

	// A valid empty ticket set with nextSequence at math.MaxInt: a
	// multi-task batch overflows the sequence space.
	seedAdmissionState(t, admissionRoot, diskAdmissionState{
		SchemaVersion: 1,
		NextSequence:  math.MaxInt,
		Tickets:       []diskAdmissionTicket{},
	})
	statePath := filepath.Join(admissionRoot, "state.json")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read seeded state.json: %v", err)
	}

	prep, err := prepareSupervisorStart(req)
	if err == nil {
		t.Fatal("prepareSupervisorStart succeeded over an overflowing sequence")
	}
	if !strings.Contains(err.Error(), "sequence overflow") {
		t.Fatalf("error %q does not mention the sequence overflow", err)
	}
	if prep != nil {
		t.Fatalf("failed prepare returned non-nil preparation %+v", prep)
	}

	// No snapshot state root was created.
	requireDirEmpty(t, backgroundRoot)

	// No ticket was added and the seeded document is byte-identical.
	after, rerr := os.ReadFile(statePath)
	if rerr != nil {
		t.Fatalf("read state.json after failed prepare: %v", rerr)
	}
	if string(after) != string(before) {
		t.Fatalf("state.json changed by failed prepare:\nbefore: %q\nafter:  %q", before, after)
	}
	st := readAdmissionState(t, admissionRoot)
	if st.NextSequence != math.MaxInt || len(st.Tickets) != 0 {
		t.Fatalf("state after failed prepare: nextSequence=%d tickets=%d, want %d and 0", st.NextSequence, len(st.Tickets), math.MaxInt)
	}
}

// TestPrepareSupervisorStartSnapshotCreateFailureLeavesZeroTickets
// pre-creates the exact run directory path as an incompatible entry — a
// plain file — so Store.Create fails after Gate.Prepare succeeded. The
// failure path must cancel the whole prepared batch: state.json ends with
// zero tickets, no Snapshot is written anywhere, and the incompatible
// entry survives untouched.
func TestPrepareSupervisorStartSnapshotCreateFailureLeavesZeroTickets(t *testing.T) {
	req, backgroundRoot, admissionRoot := newStartRequestWithTempRoots(t)

	runDirPath := filepath.Join(backgroundRoot, req.runID)
	marker := []byte("incompatible run-directory entry")
	if err := os.WriteFile(runDirPath, marker, 0o600); err != nil {
		t.Fatalf("write incompatible run dir entry: %v", err)
	}

	prep, err := prepareSupervisorStart(req)
	if err == nil {
		t.Fatal("prepareSupervisorStart succeeded although snapshot creation must fail")
	}
	if !strings.Contains(err.Error(), "create snapshot") {
		t.Fatalf("error %q does not mention snapshot creation", err)
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error %q does not mention the existing run directory", err)
	}
	if prep != nil {
		t.Fatalf("failed prepare returned non-nil preparation %+v", prep)
	}

	// Every prepared ticket was cancelled: zero remain durably queued.
	st := readAdmissionState(t, admissionRoot)
	if len(st.Tickets) != 0 {
		t.Fatalf("tickets left behind by failed prepare: %+v", st.Tickets)
	}

	// The incompatible entry is still the only content under the
	// background root, byte-identical.
	if names := dirEntryNames(t, backgroundRoot); len(names) != 1 || names[0] != req.runID {
		t.Fatalf("background root content = %v, want only the run dir entry %q", names, req.runID)
	}
	got, rerr := os.ReadFile(runDirPath)
	if rerr != nil {
		t.Fatalf("read run dir entry after failed prepare: %v", rerr)
	}
	if string(got) != string(marker) {
		t.Fatalf("run dir entry modified:\nbefore: %q\nafter:  %q", marker, got)
	}
}

// TestSupervisorPreparationRollbackRemovesSnapshotAndTickets verifies a
// successful rollback of an unconfirmed preparation: the persisted
// snapshot and its run directory disappear from the background root, and
// every prepared ticket is cancelled from admission state. A second
// rollback after that full success is clean: it must not try to remove
// the already-absent Snapshot again or re-cancel any ticket.
func TestSupervisorPreparationRollbackRemovesSnapshotAndTickets(t *testing.T) {
	req, backgroundRoot, admissionRoot := newStartRequestWithTempRoots(t)

	prep, err := prepareSupervisorStart(req)
	if err != nil {
		t.Fatalf("prepareSupervisorStart: %v", err)
	}
	if _, err := os.Stat(filepath.Join(backgroundRoot, req.runID, "snapshot.json")); err != nil {
		t.Fatalf("snapshot missing before rollback: %v", err)
	}
	if st := readAdmissionState(t, admissionRoot); len(st.Tickets) != 2 {
		t.Fatalf("tickets before rollback = %d, want 2", len(st.Tickets))
	}

	if err := prep.rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// The snapshot and its run directory are gone.
	requireDirEmpty(t, backgroundRoot)

	// Every ticket was cancelled.
	if st := readAdmissionState(t, admissionRoot); len(st.Tickets) != 0 {
		t.Fatalf("tickets left after rollback: %+v", st.Tickets)
	}

	// A repeated rollback after full success is a clean no-op.
	if err := prep.rollback(); err != nil {
		t.Fatalf("second rollback after success returned error: %v", err)
	}
	requireDirEmpty(t, backgroundRoot)
	if st := readAdmissionState(t, admissionRoot); len(st.Tickets) != 0 {
		t.Fatalf("tickets appeared after second rollback: %+v", st.Tickets)
	}
}

// TestSupervisorPreparationRollbackRemovalFailureStillCancelsTickets
// makes snapshot removal fail by adding an unexpected extra entry to the
// run directory: strict Store.Remove refuses to delete through a run
// directory that is not exactly one snapshot.json. Rollback must still
// cancel every ticket, and it must return the removal error.
func TestSupervisorPreparationRollbackRemovalFailureStillCancelsTickets(t *testing.T) {
	req, backgroundRoot, admissionRoot := newStartRequestWithTempRoots(t)

	prep, err := prepareSupervisorStart(req)
	if err != nil {
		t.Fatalf("prepareSupervisorStart: %v", err)
	}
	runDir := filepath.Join(backgroundRoot, req.runID)
	extra := filepath.Join(runDir, "unexpected.txt")
	if err := os.WriteFile(extra, []byte("unexpected"), 0o600); err != nil {
		t.Fatalf("write unexpected run dir entry: %v", err)
	}

	rollbackErr := prep.rollback()
	if rollbackErr == nil {
		t.Fatal("rollback succeeded although snapshot removal must fail")
	}
	if !strings.Contains(rollbackErr.Error(), "exactly one snapshot.json entry") {
		t.Fatalf("rollback error %q does not mention the strict removal refusal", rollbackErr)
	}

	// Strict removal refused before deleting anything: both the snapshot
	// and the unexpected entry are still present.
	if _, err := os.Stat(filepath.Join(runDir, "snapshot.json")); err != nil {
		t.Fatalf("snapshot.json missing after failed removal: %v", err)
	}
	if _, err := os.Stat(extra); err != nil {
		t.Fatalf("unexpected entry missing after failed removal: %v", err)
	}

	// Despite the removal failure, every ticket was cancelled.
	if st := readAdmissionState(t, admissionRoot); len(st.Tickets) != 0 {
		t.Fatalf("tickets left after rollback with removal failure: %+v", st.Tickets)
	}

	// Once the injected entry is gone, strict removal can succeed again:
	// a second rollback removes the Snapshot (the created flag survived
	// the failed removal) and completes cleanly, with no tickets left to
	// re-cancel.
	if err := os.Remove(extra); err != nil {
		t.Fatalf("remove injected run dir entry: %v", err)
	}
	if err := prep.rollback(); err != nil {
		t.Fatalf("second rollback after removal failure: %v", err)
	}
	requireDirEmpty(t, backgroundRoot)
	if st := readAdmissionState(t, admissionRoot); len(st.Tickets) != 0 {
		t.Fatalf("tickets left after second rollback: %+v", st.Tickets)
	}
}

// TestSupervisorPreparationNilRollbackIsSafe verifies that rollback on a
// nil preparation — and on a zero-value preparation — is a clean no-op.
func TestSupervisorPreparationNilRollbackIsSafe(t *testing.T) {
	var prep *supervisorPreparation
	if err := prep.rollback(); err != nil {
		t.Fatalf("nil rollback returned error: %v", err)
	}
	if err := (&supervisorPreparation{}).rollback(); err != nil {
		t.Fatalf("zero-value rollback returned error: %v", err)
	}
}

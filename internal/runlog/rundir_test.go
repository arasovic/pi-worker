package runlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// writeRunDir creates the run directory <dir>/<runID>/ and, when
// withSnapshot is true, a snapshot.json naming the supervisor pid the
// test chose and the terminal flag. With withRecord it also writes a
// record.jsonl without a finish line, so a reader that judged the run
// by its record instead of its snapshot would see an unfinished run.
// No owner.lock is created: the pid rule decides, through pidAlive.
func writeRunDir(t *testing.T, dir, runID string, withSnapshot bool, pid int, terminal, withRecord bool) string {
	t.Helper()
	runDir := filepath.Join(dir, runID)
	if err := os.Mkdir(runDir, 0o700); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if withSnapshot {
		data, err := json.Marshal(map[string]any{
			"schemaVersion": 1,
			"runId":         runID,
			"terminal":      terminal,
			"supervisor":    map[string]any{"pid": pid, "createTime": 0},
		})
		if err != nil {
			t.Fatalf("marshal snapshot: %v", err)
		}
		if err := os.WriteFile(filepath.Join(runDir, snapshotFileName), data, 0o600); err != nil {
			t.Fatalf("write snapshot: %v", err)
		}
	}
	if withRecord {
		path := writeRecord(t, runDir, runID, pid, false)
		if err := os.Rename(path, filepath.Join(runDir, recordFileName)); err != nil {
			t.Fatalf("rename record: %v", err)
		}
	}
	return runDir
}

// ageDir sets dir's modification time past UnknownGrace. It runs after
// every file inside is written, because writing a file refreshes it.
func ageDir(t *testing.T, dir string) {
	t.Helper()
	old := time.Now().Add(-UnknownGrace - time.Minute)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

// TestInterruptedRunDirTerminalSnapshotIsSettled asserts a run
// directory is judged by its snapshot, never by its record: a terminal
// snapshot next to a record without a finish line, whose supervisor is
// gone, is settled and moves the watermark.
func TestInterruptedRunDirTerminalSnapshotIsSettled(t *testing.T) {
	withPidAlive(t, func(int32) (bool, error) { return false, nil })
	dir := t.TempDir()
	writeRunDir(t, dir, "20260830T101500Z-1", true, 4242, true, true)

	paths, err := Interrupted(dir)
	if err != nil {
		t.Fatalf("Interrupted: %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("interrupted = %v, want none", paths)
	}
	if m := readMarker(t, dir); m.Watermark != "20260830T101500Z-1" {
		t.Fatalf("marker = %#v, want the terminal run settled", m)
	}
}

// TestInterruptedRunDirWithoutSnapshotWaitsWithinGrace asserts a run
// directory without a snapshot is a run not written yet: it is not
// reported and holds the watermark back, while a newer interrupted run
// is still found.
func TestInterruptedRunDirWithoutSnapshotWaitsWithinGrace(t *testing.T) {
	withPidAlive(t, func(int32) (bool, error) { return false, nil })
	dir := t.TempDir()
	writeRunDir(t, dir, "20260830T101500Z-1", false, 0, false, false)
	newer := writeRecord(t, dir, "20260830T102000Z-2", 4242, false)

	paths, err := Interrupted(dir)
	if err != nil {
		t.Fatalf("Interrupted: %v", err)
	}
	if want := []string{newer}; !slices.Equal(paths, want) {
		t.Fatalf("interrupted = %v, want %v", paths, want)
	}
	if m := readMarker(t, dir); m.Watermark != "" || !slices.Equal(m.Reported, []string{"20260830T102000Z-2"}) {
		t.Fatalf("marker = %#v, want the watermark held before the snapshot-less run", m)
	}
}

// TestInterruptedWarnsOnceWithLeakedRunDir asserts a snapshot-less run
// directory older than UnknownGrace — a lock-only leak — counts as
// settled: in a mix of flat records and run directories, every
// interrupted run is reported once, and the watermark passes the leak
// instead of stopping at it forever.
func TestInterruptedWarnsOnceWithLeakedRunDir(t *testing.T) {
	withPidAlive(t, func(int32) (bool, error) { return false, nil })
	dir := t.TempDir()
	flat := writeRecord(t, dir, "20260830T101500Z-1", 4242, false)
	leak := writeRunDir(t, dir, "20260830T102000Z-2", false, 0, false, false)
	ageDir(t, leak)
	runDir := writeRunDir(t, dir, "20260830T103000Z-3", true, 4243, false, true)

	paths, err := Interrupted(dir)
	if err != nil {
		t.Fatalf("Interrupted: %v", err)
	}
	if want := []string{flat, runDir}; !slices.Equal(paths, want) {
		t.Fatalf("interrupted = %v, want %v", paths, want)
	}
	if m := readMarker(t, dir); m.Watermark != "20260830T103000Z-3" || len(m.Reported) != 0 {
		t.Fatalf("marker = %#v, want the watermark past the leaked directory", m)
	}
	paths, err = Interrupted(dir)
	if err != nil {
		t.Fatalf("Interrupted: %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("second scan interrupted = %v, want none", paths)
	}
}

// TestInterruptedMixedLayoutsInRunIDOrder pins that ReadDir order is
// run id order across both layouts: <id>.jsonl sorts before <id>2/
// because '.' sorts before every digit, exactly as the id <id> sorts
// before <id>2. Each run is reported once, oldest first.
func TestInterruptedMixedLayoutsInRunIDOrder(t *testing.T) {
	withPidAlive(t, func(pid int32) (bool, error) { return pid == 4244, nil })
	dir := t.TempDir()
	flat := writeRecord(t, dir, "20260830T101500Z-1", 4242, false)
	runDir := writeRunDir(t, dir, "20260830T101500Z-12", true, 4243, false, false)
	writeRecord(t, dir, "20260830T101500Z-2", 4244, false)

	paths, err := Interrupted(dir)
	if err != nil {
		t.Fatalf("Interrupted: %v", err)
	}
	if want := []string{flat, runDir}; !slices.Equal(paths, want) {
		t.Fatalf("interrupted = %v, want %v", paths, want)
	}
	if m := readMarker(t, dir); m.Watermark != "20260830T101500Z-12" || len(m.Reported) != 0 {
		t.Fatalf("marker = %#v, want the watermark at the run directory", m)
	}
	paths, err = Interrupted(dir)
	if err != nil {
		t.Fatalf("Interrupted: %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("second scan interrupted = %v, want none", paths)
	}
}

// TestInterruptedScansRunDirPastFlatWatermark asserts a watermark left
// by a flat record does not hide a newer run directory.
func TestInterruptedScansRunDirPastFlatWatermark(t *testing.T) {
	withPidAlive(t, func(int32) (bool, error) { return false, nil })
	dir := t.TempDir()
	writeRecord(t, dir, "20260830T101500Z-1", 4242, true)
	if err := writeMarker(dir, marker{SchemaVersion: markerSchemaVersion, Watermark: "20260830T101500Z-1"}); err != nil {
		t.Fatalf("writeMarker: %v", err)
	}
	runDir := writeRunDir(t, dir, "20260830T102000Z-2", true, 4243, false, false)

	paths, err := Interrupted(dir)
	if err != nil {
		t.Fatalf("Interrupted: %v", err)
	}
	if want := []string{runDir}; !slices.Equal(paths, want) {
		t.Fatalf("interrupted = %v, want %v", paths, want)
	}
}

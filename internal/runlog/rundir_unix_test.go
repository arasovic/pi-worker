//go:build darwin || linux

package runlog

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

// holdOwnerLock takes runDir's owner lock for the rest of the test.
func holdOwnerLock(t *testing.T, runDir string) {
	t.Helper()
	f, err := AcquireOwnerLock(runDir)
	if err != nil {
		t.Fatalf("AcquireOwnerLock: %v", err)
	}
	t.Cleanup(func() { f.Close() })
}

// releasedOwnerLock leaves runDir with an owner lock file nobody holds:
// the owner is gone.
func releasedOwnerLock(t *testing.T, runDir string) {
	t.Helper()
	f, err := AcquireOwnerLock(runDir)
	if err != nil {
		t.Fatalf("AcquireOwnerLock: %v", err)
	}
	f.Close()
}

// TestInterruptedRunDirLockHeldIsRunning asserts a held owner lock
// keeps a non-terminal run directory running although its recorded
// supervisor pid is dead: it is not reported and stops the watermark,
// while the newer interrupted run behind it is still found.
func TestInterruptedRunDirLockHeldIsRunning(t *testing.T) {
	withPidAlive(t, func(int32) (bool, error) { return false, nil })
	dir := t.TempDir()
	runDir := writeRunDir(t, dir, "20260830T101500Z-1", true, 4242, false, false)
	holdOwnerLock(t, runDir)
	newer := writeRecord(t, dir, "20260830T102000Z-2", 4243, false)

	paths, err := Interrupted(dir)
	if err != nil {
		t.Fatalf("Interrupted: %v", err)
	}
	if want := []string{newer}; !slices.Equal(paths, want) {
		t.Fatalf("interrupted = %v, want %v", paths, want)
	}
	if m := readMarker(t, dir); m.Watermark != "" {
		t.Fatalf("marker = %#v, want the watermark held at the locked run", m)
	}
}

// writeLeftoverRunDir writes <dir>/<runID>/record.jsonl without a
// finish line, carrying the given worker lines.
func writeLeftoverRunDir(t *testing.T, dir, runID string, startPID int, workers ...workerSpec) (runDir, path string) {
	t.Helper()
	runDir = filepath.Join(dir, runID)
	if err := os.Mkdir(runDir, 0o700); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	flat := writeLeftoverRecord(t, runDir, runID, startPID, false, workers...)
	path = filepath.Join(runDir, recordFileName)
	if err := os.Rename(flat, path); err != nil {
		t.Fatalf("rename record: %v", err)
	}
	return runDir, path
}

// TestLeftoversRunDirSettledByOwnerLock asserts a run directory's
// record is settled by its owner lock, not its start line's pid: a free
// lock reports the leftover although the pid is alive, and a held lock
// reports nothing although the pid is dead.
func TestLeftoversRunDirSettledByOwnerLock(t *testing.T) {
	withLiveProcesses(t, []liveProcess{{pid: 5010, pgid: 5001, createTime: 1500}}, nil)
	const runID = "20260830T101500Z-1"

	t.Run("lock free", func(t *testing.T) {
		withPidAlive(t, func(int32) (bool, error) { return true, nil })
		dir := t.TempDir()
		runDir, path := writeLeftoverRunDir(t, dir, runID, 4242, workerSpec{pid: 5001, createTime: 1000})
		releasedOwnerLock(t, runDir)

		leftovers, err := Leftovers(dir)
		if err != nil {
			t.Fatalf("Leftovers: %v", err)
		}
		want := []Leftover{{RunID: runID, Path: path, PIDs: []int{5010}}}
		if !reflect.DeepEqual(leftovers, want) {
			t.Fatalf("leftovers = %#v, want %#v", leftovers, want)
		}
	})

	t.Run("lock held", func(t *testing.T) {
		withPidAlive(t, func(int32) (bool, error) { return false, nil })
		dir := t.TempDir()
		runDir, _ := writeLeftoverRunDir(t, dir, runID, 4242, workerSpec{pid: 5001, createTime: 1000})
		holdOwnerLock(t, runDir)

		leftovers, err := Leftovers(dir)
		if err != nil {
			t.Fatalf("Leftovers: %v", err)
		}
		if len(leftovers) != 0 {
			t.Fatalf("leftovers = %#v, want none while the owner lock is held", leftovers)
		}
	})
}

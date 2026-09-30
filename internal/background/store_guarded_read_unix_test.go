//go:build darwin || linux

package background

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// validSnapshotBytes returns the encoded bytes of a fully valid snapshot
// for makeRunID(fixtureTime): content Load accepts, so a test that swaps
// these bytes in over snapshot.json can only be refused by the post-open
// same-file re-check — without it, Load would open the replacement and
// return it successfully.
func validSnapshotBytes(t *testing.T) []byte {
	t.Helper()
	data, err := encodeSnapshot(buildValidSnapshot(t))
	if err != nil {
		t.Fatalf("encode valid snapshot: %v", err)
	}
	return data
}

// TestLoadRefusesOversizedSnapshot pins the read ceiling: a snapshot grown
// past maxSnapshotBytes is refused before it is read, and the refusal
// leaves the file on disk untouched.
func TestLoadRefusesOversizedSnapshot(t *testing.T) {
	root := t.TempDir()
	runID := makeRunID(fixtureTime)
	store := setUpRunDir(t, root, runID, validSnapshotBytes(t))
	if _, err := store.Load(runID); err != nil {
		t.Fatalf("baseline Load: %v", err)
	}

	snapPath := snapshotPath(root, runID)
	if err := os.Truncate(snapPath, maxSnapshotBytes+1); err != nil {
		t.Fatalf("truncate snapshot: %v", err)
	}

	_, err := store.Load(runID)
	if err == nil {
		t.Fatal("expected error for oversized snapshot")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("want 'too large' refusal, got: %v", err)
	}
	if !strings.Contains(err.Error(), strconv.FormatInt(maxSnapshotBytes+1, 10)) {
		t.Errorf("want refusal to name the size, got: %v", err)
	}

	fi, statErr := os.Stat(snapPath)
	if statErr != nil {
		t.Fatalf("stat snapshot after refused Load: %v", statErr)
	}
	if fi.Size() != maxSnapshotBytes+1 {
		t.Errorf("snapshot size = %d; want unchanged %d", fi.Size(), maxSnapshotBytes+1)
	}
}

// TestLoadRefusesSnapshotSwappedBeforeOpen pins the post-open re-check: a
// different regular file renamed over snapshot.json between the Lstat
// checks and the open is refused rather than read. Without the SameFile
// re-check, Load would open and return the replacement. The decoy holds
// the snapshot's exact bytes, so the pre-open regular-file and size arms
// stay silent, and it is a regular file like the original, so only the
// same-file re-check can refuse it.
func TestLoadRefusesSnapshotSwappedBeforeOpen(t *testing.T) {
	root := t.TempDir()
	runID := makeRunID(fixtureTime)
	store := setUpRunDir(t, root, runID, validSnapshotBytes(t))
	if _, err := store.Load(runID); err != nil {
		t.Fatalf("baseline Load: %v", err)
	}

	snapPath := snapshotPath(root, runID)
	swapPath := filepath.Join(root, "swap.json")
	if err := os.WriteFile(swapPath, validSnapshotBytes(t), 0o600); err != nil {
		t.Fatalf("write swap file: %v", err)
	}

	t.Cleanup(func() { beforeSnapshotOpen = func() {} })
	beforeSnapshotOpen = func() {
		if err := os.Rename(swapPath, snapPath); err != nil {
			t.Errorf("rename swap over snapshot: %v", err)
		}
	}

	_, err := store.Load(runID)
	if err == nil {
		t.Fatal("expected error for snapshot swapped before open")
	}
	if !strings.Contains(err.Error(), "changed before reading") {
		t.Errorf("want 'changed before reading' refusal, got: %v", err)
	}
}

// TestLoadRefusesPipeSwappedInBeforeOpen pins the non-blocking open: a
// named pipe renamed over snapshot.json between the Lstat checks and the
// open must produce an error, not block forever waiting for a writer that
// never comes. Load runs in a goroutine under a 2 s deadline so a
// regression (blocking open) fails the test instead of hanging the suite.
func TestLoadRefusesPipeSwappedInBeforeOpen(t *testing.T) {
	root := t.TempDir()
	runID := makeRunID(fixtureTime)
	store := setUpRunDir(t, root, runID, validSnapshotBytes(t))
	if _, err := store.Load(runID); err != nil {
		t.Fatalf("baseline Load: %v", err)
	}

	snapPath := snapshotPath(root, runID)
	fifoPath := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	t.Cleanup(func() { beforeSnapshotOpen = func() {} })
	var swapErr error
	beforeSnapshotOpen = func() {
		swapErr = os.Rename(fifoPath, snapPath)
	}

	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		_, err := store.Load(runID)
		done <- result{err}
	}()

	select {
	case r := <-done:
		if swapErr != nil {
			t.Fatalf("rename pipe over snapshot: %v", swapErr)
		}
		if r.err == nil {
			t.Fatal("expected error for pipe swapped in before open")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Load blocked on a writerless pipe: expected an error within 2s")
	}
}

// TestLoadMismatchErrorBoundsTheQuotedRunID pins the bound on the runId
// echoed by the mismatch error: a 100,000-rune decoded runId must not be
// quoted whole into the error.
func TestLoadMismatchErrorBoundsTheQuotedRunID(t *testing.T) {
	root := t.TempDir()
	runID := makeRunID(fixtureTime)
	runDir := filepath.Join(root, runID)
	if err := os.Mkdir(runDir, 0o700); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	content := `{"schemaVersion":1,"runId":"` + strings.Repeat("a", 100000) + `"}`
	if err := os.WriteFile(snapshotPath(root, runID), []byte(content), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	_, err = store.Load(runID)
	if err == nil {
		t.Fatal("expected runId mismatch error")
	}
	if !strings.Contains(err.Error(), "does not match requested") {
		t.Fatalf("want 'does not match requested', got: %v", err)
	}
	if n := len(err.Error()); n >= 1024 {
		t.Errorf("mismatch error is %d bytes; want below 1024", n)
	}
}

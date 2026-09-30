//go:build darwin || linux

package background

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/contracts"
	"github.com/arasovic/pi-worker/internal/run"
	"github.com/arasovic/pi-worker/internal/runlog"
)

// TestManagerCancelTerminalSnapshotDoesNotSignal proves that a terminal
// snapshot is returned as-is without consulting or signalling its supervisor.
func TestManagerCancelTerminalSnapshotDoesNotSignal(t *testing.T) {
	created, err := supervisorPidCreateTime(os.Getpid())
	if err != nil {
		t.Fatalf("observe test process: %v", err)
	}

	snap, err := NewSnapshot(
		makeRunID(fixtureTime),
		fixtureTime,
		t.TempDir(),
		ProcessIdentity{PID: os.Getpid(), CreateTime: created},
		[]run.Task{fixtureTask()},
		time.Minute,
		nil,
	)
	if err != nil {
		t.Fatalf("build snapshot: %v", err)
	}
	finishedAt := fixtureTime.Add(time.Second)
	snap.State = RunCompleted
	snap.Terminal = true
	snap.UpdatedAt = finishedAt
	snap.Status = ptrRunStatus(contracts.RunCompleted)
	outcome := contracts.OutcomeCompleted
	snap.Outcome = &outcome
	snap.Workers[0].State = WorkerCompleted
	snap.Workers[0].FinishedAt = &finishedAt
	if err := snap.Validate(); err != nil {
		t.Fatalf("build terminal snapshot: %v", err)
	}

	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := createUnlockedSnapshot(store, snap); err != nil {
		t.Fatalf("store terminal snapshot: %v", err)
	}
	manager, err := NewManager(root, "", t.TempDir(), 1)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	oldSignal := supervisorSignal
	called := 0
	supervisorSignal = func(pid int) error {
		called++
		return nil
	}
	t.Cleanup(func() { supervisorSignal = oldSignal })

	got, err := manager.Cancel(snap.RunID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !reflect.DeepEqual(got, snap) {
		t.Fatalf("Cancel returned a changed snapshot:\n got: %+v\nwant: %+v", got, snap)
	}
	if called != 0 {
		t.Fatalf("supervisorSignal called %d times, want none", called)
	}
}

// countSignals replaces the signal seam for the rest of the test and
// returns the number of signals it was asked to send.
func countSignals(t *testing.T) *int {
	t.Helper()
	oldSignal := supervisorSignal
	called := 0
	supervisorSignal = func(int) error {
		called++
		return nil
	}
	t.Cleanup(func() { supervisorSignal = oldSignal })
	return &called
}

// TestManagerCancelSignalsOnlyWhenOwnerLockIsHeld requires that Cancel
// signals a live supervisor whose owner lock is held or whose run directory
// has no owner lock (the pid rule decides), and signals nothing
// when the lock is free or cannot be probed, although the recorded pid is
// the live test process in every case.
func TestManagerCancelSignalsOnlyWhenOwnerLockIsHeld(t *testing.T) {
	for _, tc := range []struct {
		name       string
		arrange    func(t *testing.T, runDir string)
		wantSignal bool
	}{
		{"held", holdOwnerLock, true},
		{"absent", func(*testing.T, string) {}, true},
		{"free", leaveOwnerLockFree, false},
		{"unknown", func(t *testing.T, runDir string) {
			// O_NOFOLLOW refuses a symbolic link, so the probe cannot tell.
			if err := os.Symlink(filepath.Join(runDir, "missing"), filepath.Join(runDir, runlog.OwnerLockName)); err != nil {
				t.Fatalf("symlink owner lock: %v", err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			runID := storeLiveSnapshot(t, root, liveIdentity(t))
			tc.arrange(t, filepath.Join(root, runID))
			manager, err := NewManager(root, "", t.TempDir(), 1)
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			called := countSignals(t)

			_, err = manager.Cancel(runID)
			if tc.wantSignal {
				if err != nil || *called != 1 {
					t.Fatalf("Cancel = %v with %d signals, want nil and one signal", err, *called)
				}
				return
			}
			var unavailable *SupervisorUnavailableError
			if !errors.As(err, &unavailable) || *called != 0 {
				t.Fatalf("Cancel = %v with %d signals, want *SupervisorUnavailableError and none", err, *called)
			}
		})
	}
}

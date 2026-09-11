//go:build darwin || linux

package background

import (
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/contracts"
	"github.com/arasovic/pi-worker/internal/run"
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
	if err := store.Create(snap); err != nil {
		t.Fatalf("store terminal snapshot: %v", err)
	}
	manager, err := NewManager(root, t.TempDir(), 1)
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

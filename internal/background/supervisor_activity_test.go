//go:build darwin || linux

package background

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/pi"
)

// TestSupervisorRunObserverActivityTracksRunningWorker verifies that an
// activity report lands on a durably running worker, bumps the snapshot's
// UpdatedAt, and leaves a queued sibling untouched.
func TestSupervisorRunObserverActivityTracksRunningWorker(t *testing.T) {
	req, _, _ := newStartRequestWithTempRoots(t)
	prep, err := prepareSupervisorStart(req)
	if err != nil {
		t.Fatalf("prepareSupervisorStart: %v", err)
	}
	observer := newSupervisorRunObserver(prep.store, prep.snapshot)

	// Promote worker 1 with this live process's identity so the durable
	// snapshot has a running worker to report activity against.
	observer.observer()(1, os.Getpid())
	before, err := prep.store.Load(req.runID)
	if err != nil {
		t.Fatalf("load running snapshot: %v", err)
	}
	if before.Workers[0].State != WorkerRunning {
		t.Fatalf("worker 1 state = %q, want running", before.Workers[0].State)
	}
	if before.Workers[0].Activity != nil {
		t.Fatalf("worker 1 activity = %+v before any report, want nil", before.Workers[0].Activity)
	}

	activityAt := time.Now().UTC().Truncate(time.Second)
	observer.activity()(1, pi.Activity{LastEventAt: activityAt, LastTool: "bash", ToolCalls: 3})

	got, err := prep.store.Load(req.runID)
	if err != nil {
		t.Fatalf("load snapshot after activity: %v", err)
	}
	if got.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("updatedAt = %v, want it bumped past %v", got.UpdatedAt, before.UpdatedAt)
	}
	worker := got.Workers[0]
	if worker.Activity == nil {
		t.Fatal("worker 1 activity must be durable after a report")
	}
	if worker.Activity.ToolCalls != 3 || worker.Activity.LastTool != "bash" {
		t.Fatalf("worker 1 activity = %+v, want toolCalls 3 lastTool bash", worker.Activity)
	}
	if !worker.Activity.LastEventAt.Equal(activityAt) {
		t.Fatalf("worker 1 lastEventAt = %v, want %v", worker.Activity.LastEventAt, activityAt)
	}
	// The queued sibling saw nothing: activity is per worker, and a worker
	// that has not started must not be given one.
	if sibling := got.Workers[1]; sibling.State != WorkerQueued || sibling.Activity != nil {
		t.Fatalf("worker 2 = %+v, want queued with no activity", sibling)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("snapshot with activity failed validation: %v", err)
	}
}

// TestSupervisorRunObserverActivityReplaceFailureIsSilent verifies that a
// failed activity Replace is best effort: it adds nothing to failures()
// (which fail the run) and leaves the durable snapshot untouched.
func TestSupervisorRunObserverActivityReplaceFailureIsSilent(t *testing.T) {
	req, _, _ := newStartRequestWithTempRoots(t)
	prep, err := prepareSupervisorStart(req)
	if err != nil {
		t.Fatalf("prepareSupervisorStart: %v", err)
	}
	observer := newSupervisorRunObserver(prep.store, prep.snapshot)
	observer.observer()(1, os.Getpid())
	if got := observer.failures(); len(got) != 0 {
		t.Fatalf("failures after a successful launch = %v, want none", got)
	}

	// Remove the run directory so the next Replace cannot succeed.
	if err := os.RemoveAll(filepath.Join(prep.store.root, string(req.runID))); err != nil {
		t.Fatalf("remove run directory: %v", err)
	}
	observer.activity()(1, pi.Activity{LastEventAt: time.Now().UTC(), ToolCalls: 1})

	if got := observer.failures(); len(got) != 0 {
		t.Fatalf("failures after a dropped activity report = %v, want none", got)
	}
}

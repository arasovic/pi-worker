//go:build darwin || linux

package run

import (
	"context"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/admission"
	"github.com/arasovic/pi-worker/internal/contracts"
	"github.com/arasovic/pi-worker/internal/pi"
)

func TestControllerForegroundAdmissionStartsExecutionClockAfterLease(t *testing.T) {
	gate, err := admission.Open(t.TempDir(), 1)
	if err != nil {
		t.Fatalf("open gate: %v", err)
	}

	// Step 2: hold one lease so the controller ticket queues but cannot grant.
	blocker, err := gate.Enqueue(admission.Request{RunID: "blocker", WorkerID: 1})
	if err != nil {
		t.Fatalf("enqueue blocker: %v", err)
	}
	blockerLease, err := blocker.Wait(context.Background())
	if err != nil {
		t.Fatalf("wait blocker: %v", err)
	}
	t.Cleanup(func() { blockerLease.Release() })

	// Step 3: one-task controller with foreground admission.
	worker := newScriptedWorker()
	executionTimeout := 5 * time.Minute
	acceptedAt := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	controller := New(worker, WithForegroundAdmission(gate, "run-1", acceptedAt, executionTimeout))

	// Steps 4-5: intercept the deadline/duration sent by the admission goroutines.
	queueDeadlineCh := make(chan time.Time, 1)
	controller.queueContext = func(parent context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
		queueDeadlineCh <- deadline
		return context.WithCancel(parent)
	}

	execDurationCh := make(chan time.Duration, 1)
	controller.executionContext = func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		execDurationCh <- d
		return context.WithCancel(parent)
	}

	// Step 6: start Run in a goroutine.
	runDone := make(chan struct{})
	var result Result
	var runErr error
	go func() {
		defer close(runDone)
		result, runErr = controller.Run(context.Background(), validRequest("task-1"))
	}()

	// Step 7: receive the queue deadline and assert exact equality.
	select {
	case got := <-queueDeadlineCh:
		want := acceptedAt.Add(foregroundQueueTimeout)
		if got != want {
			t.Fatalf("queue deadline = %v, want %v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for queue deadline")
	}

	// Step 8: execution context must not have been sent yet while blocker is held.
	select {
	case d := <-execDurationCh:
		t.Fatalf("execution context sent while blocker lease held: duration %v", d)
	default:
		// Expected: channel empty.
	}

	// Step 9: release blocker; receive execution duration and assert.
	if err := blockerLease.Release(); err != nil {
		t.Fatalf("release blocker lease: %v", err)
	}
	select {
	case got := <-execDurationCh:
		if got != executionTimeout {
			t.Fatalf("execution duration = %v, want %v", got, executionTimeout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for execution context duration")
	}

	// Step 10: wait for Run, assert no error and completed status.
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	if runErr != nil {
		t.Fatalf("Run returned error: %v", runErr)
	}
	if result.Status != contracts.RunCompleted {
		t.Fatalf("status = %q, want %q", result.Status, contracts.RunCompleted)
	}
	if len(result.Workers) != 1 {
		t.Fatalf("workers = %d, want 1", len(result.Workers))
	}
	if result.Workers[0].Status != pi.StatusCompleted {
		t.Fatalf("worker status = %q, want %q", result.Workers[0].Status, pi.StatusCompleted)
	}
	if worker.callCount() != 1 {
		t.Fatalf("worker call count = %d, want 1", worker.callCount())
	}

	// Step 11: probe proves the task lease did not remain.
	probe, err := gate.Enqueue(admission.Request{RunID: "probe", WorkerID: 1})
	if err != nil {
		t.Fatalf("enqueue probe: %v", err)
	}
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer probeCancel()
	probeLease, err := probe.Wait(probeCtx)
	if err != nil {
		t.Fatalf("probe wait: %v", err)
	}
	if err := probeLease.Release(); err != nil {
		t.Fatalf("release probe lease: %v", err)
	}
}

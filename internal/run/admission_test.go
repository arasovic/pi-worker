//go:build darwin || linux

package run

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/admission"
	"github.com/arasovic/pi-worker/internal/contracts"
	"github.com/arasovic/pi-worker/internal/pi"
)

// admissionOrderWorker is a minimal scripted worker for admission-order
// tests: it reports when each worker starts, blocks until the test
// releases that worker or its context ends, and records the outcome as
// completed with a done-<workerID> explanation on release, or as
// timed-out/cancelled using the existing Pi statuses when the context
// ends first.
type admissionOrderWorker struct {
	started  chan int
	releases map[int]<-chan struct{}
}

func (w *admissionOrderWorker) Run(ctx context.Context, req pi.WorkerRequest) pi.WorkerResult {
	w.started <- req.WorkerID
	workerID := strconv.Itoa(req.WorkerID)
	select {
	case <-w.releases[req.WorkerID]:
		return pi.WorkerResult{Model: req.Model, Status: pi.StatusCompleted, Explanation: "done-" + workerID}
	case <-ctx.Done():
		if ctx.Err() == context.DeadlineExceeded {
			return pi.WorkerResult{Model: req.Model, Status: pi.StatusTimedOut}
		}
		return pi.WorkerResult{Model: req.Model, Status: pi.StatusCancelled}
	}
}

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
	controller.queueContext = func(parent context.Context, _ int, deadline time.Time) (context.Context, context.CancelFunc) {
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

// TestControllerForegroundAdmissionKeepsLaterRunBehindAllTasks verifies that
// a later enqueued outsider ticket cannot obtain a lease until all of a
// controller's admitted tasks have completed and released their leases.
// With maxLive=1 the sequence is: blocker → task-1 → task-2 → outsider.
func TestControllerForegroundAdmissionKeepsLaterRunBehindAllTasks(t *testing.T) {
	type outsiderResult struct {
		lease *admission.Lease
		err   error
	}

	testCtx, testCancel := context.WithCancel(context.Background())

	// Step 1: gate with capacity 1.
	gate, err := admission.Open(t.TempDir(), 1)
	if err != nil {
		t.Fatalf("open gate: %v", err)
	}

	var (
		blockerLease       *admission.Lease
		outsiderTicket     *admission.QueueTicket
		outsiderCh         chan outsiderResult
		earlyOutsiderLease *admission.Lease
		probeTicket        *admission.QueueTicket
		probeLease         *admission.Lease
		runStarted         bool
	)
	runDone := make(chan struct{})

	t.Cleanup(func() {
		testCancel()
		if blockerLease != nil {
			blockerLease.Release()
		}
		if outsiderTicket != nil {
			outsiderTicket.Cancel()
		}
		if probeTicket != nil {
			probeTicket.Cancel()
		}
		if probeLease != nil {
			probeLease.Release()
		}
		if earlyOutsiderLease != nil {
			earlyOutsiderLease.Release()
		}
		if outsiderCh != nil {
			select {
			case res := <-outsiderCh:
				if res.lease != nil {
					res.lease.Release()
				}
			default:
			}
		}
		if runStarted {
			select {
			case <-runDone:
			case <-time.After(5 * time.Second):
			}
		}
	})

	// Step 2: acquire blocker lease so controller tickets queue behind it.
	blocker, err := gate.Enqueue(admission.Request{RunID: "blocker", WorkerID: 1})
	if err != nil {
		t.Fatalf("enqueue blocker: %v", err)
	}
	bl, err := blocker.Wait(context.Background())
	if err != nil {
		t.Fatalf("wait blocker: %v", err)
	}
	blockerLease = bl

	// Step 3: admitted controller with two tasks.
	relCh1 := make(chan struct{})
	relCh2 := make(chan struct{})
	worker := &admissionOrderWorker{
		started:  make(chan int, 2),
		releases: map[int]<-chan struct{}{1: relCh1, 2: relCh2},
	}
	acceptedAt := time.Now()
	controller := New(worker, WithForegroundAdmission(gate, "run-1", acceptedAt, 5*time.Minute))

	// Step 4: override queueContext to send a marker then return a cancellable context.
	queueEntered := make(chan struct{}, 1)
	controller.queueContext = func(parent context.Context, _ int, _ time.Time) (context.Context, context.CancelFunc) {
		queueEntered <- struct{}{}
		return context.WithCancel(parent)
	}

	// Step 5: start Run in a goroutine.
	var result Result
	var runErr error
	runStarted = true
	go func() {
		defer close(runDone)
		result, runErr = controller.Run(testCtx, validRequest("task-1", "task-2"))
	}()

	// Step 6: receive the first queue marker (confirms controller started enqueuing).
	select {
	case <-queueEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for queue marker")
	}

	// Enqueue a later outsider ticket and start its Wait concurrently.
	ot, err := gate.Enqueue(admission.Request{RunID: "outsider", WorkerID: 1})
	if err != nil {
		t.Fatalf("enqueue outsider: %v", err)
	}
	outsiderTicket = ot
	outsiderCh = make(chan outsiderResult, 1)
	go func() {
		octx, ocancel := context.WithTimeout(testCtx, 5*time.Second)
		defer ocancel()
		lease, err := outsiderTicket.Wait(octx)
		outsiderCh <- outsiderResult{lease, err}
	}()

	// Step 7: release blocker; controller tasks can now acquire leases.
	if err := blockerLease.Release(); err != nil {
		t.Fatalf("release blocker lease: %v", err)
	}

	// Step 8: receive first worker start; must be task-1.
	select {
	case id := <-worker.started:
		if id != 1 {
			t.Fatalf("first worker started = %d, want 1", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first worker start")
	}

	// Outsider must not have been admitted while task-1 still holds the lease.
	select {
	case res := <-outsiderCh:
		earlyOutsiderLease = res.lease
		t.Fatalf("outsider result arrived during task-1: lease=%v, err=%v", res.lease, res.err)
	default:
	}

	// Step 9: close release-1 so task-1 finishes and releases its lease.
	close(relCh1)

	// Step 10: receive second worker start; must be task-2.
	select {
	case id := <-worker.started:
		if id != 2 {
			t.Fatalf("second worker started = %d, want 2", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for second worker start")
	}

	// Outsider must not have been admitted while task-2 holds the lease.
	select {
	case res := <-outsiderCh:
		earlyOutsiderLease = res.lease
		t.Fatalf("outsider result arrived during task-2: lease=%v, err=%v", res.lease, res.err)
	default:
	}

	// Step 11: close release-2 so task-2 finishes and releases its lease.
	close(relCh2)

	// Step 12: outsider can now be admitted after both tasks released.
	var or outsiderResult
	select {
	case or = <-outsiderCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for outsider result")
	}
	if or.err != nil {
		t.Fatalf("outsider Wait: %v", or.err)
	}
	if or.lease == nil {
		t.Fatal("outsider lease is nil")
	}
	if err := or.lease.Release(); err != nil {
		t.Fatalf("release outsider lease: %v", err)
	}

	// Step 13: wait for Controller result.
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
	if len(result.Workers) != 2 {
		t.Fatalf("workers = %d, want 2", len(result.Workers))
	}
	wantExplanations := []string{"done-1", "done-2"}
	for i, w := range result.Workers {
		if w.Explanation != wantExplanations[i] {
			t.Fatalf("worker %d explanation = %q, want %q", i, w.Explanation, wantExplanations[i])
		}
	}

	// Step 14: enqueue and wait on a final probe with a bounded context.
	probe, err := gate.Enqueue(admission.Request{RunID: "probe", WorkerID: 1})
	if err != nil {
		t.Fatalf("enqueue probe: %v", err)
	}
	probeTicket = probe
	pctx, pcancel := context.WithTimeout(testCtx, 5*time.Second)
	defer pcancel()
	pl, perr := probe.Wait(pctx)
	if perr != nil {
		t.Fatalf("probe wait: %v", perr)
	}
	probeLease = pl
	if err := probeLease.Release(); err != nil {
		t.Fatalf("release probe lease: %v", err)
	}
}

func TestControllerForegroundAdmissionQueueTimeoutAggregation(t *testing.T) {
	boundedProbe := func(t *testing.T, gate *admission.Gate) {
		t.Helper()
		tk, err := gate.Enqueue(admission.Request{RunID: "probe", WorkerID: 1})
		if err != nil {
			t.Fatalf("enqueue probe: %v", err)
		}
		pctx, pcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer pcancel()
		pl, err := tk.Wait(pctx)
		if err != nil {
			t.Fatalf("probe wait: %v", err)
		}
		if err := pl.Release(); err != nil {
			t.Fatalf("release probe: %v", err)
		}
	}

	expiredCtx := func(parent context.Context, _ int, _ time.Time) (context.Context, context.CancelFunc) {
		return context.WithDeadline(parent, time.Now().Add(-time.Second))
	}

	t.Run("all timed out", func(t *testing.T) {
		gate, err := admission.Open(t.TempDir(), 1)
		if err != nil {
			t.Fatalf("open gate: %v", err)
		}

		worker := newScriptedWorker()
		acceptedAt := time.Now()
		controller := New(worker, WithForegroundAdmission(gate, "run-1", acceptedAt, 5*time.Minute))
		controller.queueContext = expiredCtx

		result, runErr := controller.Run(context.Background(), validRequest("task-1", "task-2"))

		if runErr != nil {
			t.Fatalf("Run returned error: %v", runErr)
		}
		if result.Status != contracts.RunTimedOut {
			t.Fatalf("status = %q, want %q", result.Status, contracts.RunTimedOut)
		}
		if worker.callCount() != 0 {
			t.Fatalf("worker call count = %d, want 0", worker.callCount())
		}
		if len(result.Workers) != 2 {
			t.Fatalf("workers = %d, want 2", len(result.Workers))
		}
		for i, w := range result.Workers {
			if w.Status != pi.StatusTimedOut {
				t.Fatalf("worker %d status = %q, want %q", i+1, w.Status, pi.StatusTimedOut)
			}
			if w.Model != "acme/m-1" {
				t.Fatalf("worker %d model = %q, want %q", i+1, w.Model, "acme/m-1")
			}
		}

		boundedProbe(t, gate)
	})

	t.Run("completed and timed out is partial", func(t *testing.T) {
		gate, err := admission.Open(t.TempDir(), 1)
		if err != nil {
			t.Fatalf("open gate: %v", err)
		}

		worker := newScriptedWorker()
		acceptedAt := time.Now()
		controller := New(worker, WithForegroundAdmission(gate, "run-1", acceptedAt, 5*time.Minute))
		controller.queueContext = func(parent context.Context, workerID int, _ time.Time) (context.Context, context.CancelFunc) {
			if workerID == 1 {
				return context.WithCancel(parent)
			}
			return context.WithDeadline(parent, time.Now().Add(-time.Second))
		}

		result, runErr := controller.Run(context.Background(), validRequest("task-1", "task-2"))

		if runErr != nil {
			t.Fatalf("Run returned error: %v", runErr)
		}
		if result.Status != contracts.RunPartial {
			t.Fatalf("status = %q, want %q", result.Status, contracts.RunPartial)
		}
		if worker.callCount() != 1 {
			t.Fatalf("worker call count = %d, want 1", worker.callCount())
		}
		if len(result.Workers) != 2 {
			t.Fatalf("workers = %d, want 2", len(result.Workers))
		}
		// Worker 1 completed.
		if result.Workers[0].Status != pi.StatusCompleted {
			t.Fatalf("worker 1 status = %q, want %q", result.Workers[0].Status, pi.StatusCompleted)
		}
		if result.Workers[0].Explanation != "done:task-1" {
			t.Fatalf("worker 1 explanation = %q, want %q", result.Workers[0].Explanation, "done:task-1")
		}
		// Worker 2 timed out.
		if result.Workers[1].Status != pi.StatusTimedOut {
			t.Fatalf("worker 2 status = %q, want %q", result.Workers[1].Status, pi.StatusTimedOut)
		}
		if result.Workers[1].Model != "acme/m-1" {
			t.Fatalf("worker 2 model = %q, want %q", result.Workers[1].Model, "acme/m-1")
		}

		boundedProbe(t, gate)
	})
}

// TestControllerForegroundAdmissionParentCancelCleansQueuedTickets verifies
// that cancelling the parent context while controller tasks are still queued
// behind a held blocker lease cancels every queued ticket out of the gate:
// the controller returns RunCancelled with cancelled worker results, the
// worker is never called, and a bounded probe afterwards proves both queued
// tickets were removed.
func TestControllerForegroundAdmissionParentCancelCleansQueuedTickets(t *testing.T) {
	parentCtx, parentCancel := context.WithCancel(context.Background())

	// Step 1: gate with capacity 1.
	gate, err := admission.Open(t.TempDir(), 1)
	if err != nil {
		t.Fatalf("open gate: %v", err)
	}

	// Step 2: acquire a blocker lease so controller tickets queue behind it.
	blocker, err := gate.Enqueue(admission.Request{RunID: "blocker", WorkerID: 1})
	if err != nil {
		t.Fatalf("enqueue blocker: %v", err)
	}
	blockerLease, err := blocker.Wait(context.Background())
	if err != nil {
		t.Fatalf("wait blocker: %v", err)
	}

	// Step 3: admitted controller with two tasks.
	worker := newScriptedWorker()
	controller := New(worker, WithForegroundAdmission(gate, "run-1", time.Now(), 5*time.Minute))

	// Step 4: override queueContext to send each worker ID then return a
	// cancellable context derived from the parent.
	queuedWorkers := make(chan int, 2)
	controller.queueContext = func(parent context.Context, workerID int, _ time.Time) (context.Context, context.CancelFunc) {
		queuedWorkers <- workerID
		return context.WithCancel(parent)
	}

	// Step 5: start Run in a goroutine with the cancellable parent.
	runDone := make(chan struct{})
	var result Result
	var runErr error
	runStarted := false
	t.Cleanup(func() {
		parentCancel()
		if blockerLease != nil {
			blockerLease.Release()
		}
		if runStarted {
			select {
			case <-runDone:
			case <-time.After(5 * time.Second):
			}
		}
	})
	go func() {
		defer close(runDone)
		result, runErr = controller.Run(parentCtx, validRequest("task-1", "task-2"))
	}()
	runStarted = true

	// Step 6: receive both worker IDs, proving both task waits are queued.
	// The two goroutines race to their queueContext override, so the IDs
	// may arrive in either order; only the set matters.
	seen := make(map[int]bool, 2)
	for len(seen) < 2 {
		select {
		case got := <-queuedWorkers:
			if got != 1 && got != 2 {
				t.Fatalf("queued worker id = %d, want 1 or 2", got)
			}
			seen[got] = true
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for queued worker")
		}
	}

	// No worker may have been called while both tasks are still queued.
	if worker.callCount() != 0 {
		t.Fatalf("worker call count = %d, want 0 while queued", worker.callCount())
	}

	// Step 7: cancel the parent; queued waits end and tickets are removed.
	parentCancel()

	// Step 8: wait for Run with a guard.
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after parent cancel")
	}
	if runErr != nil {
		t.Fatalf("Run returned error: %v", runErr)
	}
	if result.Status != contracts.RunCancelled {
		t.Fatalf("status = %q, want %q", result.Status, contracts.RunCancelled)
	}
	if worker.callCount() != 0 {
		t.Fatalf("worker call count = %d, want 0", worker.callCount())
	}
	if len(result.Workers) != 2 {
		t.Fatalf("workers = %d, want 2", len(result.Workers))
	}
	for i, w := range result.Workers {
		if w.Model != "acme/m-1" {
			t.Fatalf("worker %d model = %q, want %q", i+1, w.Model, "acme/m-1")
		}
		if w.Status != pi.StatusCancelled {
			t.Fatalf("worker %d status = %q, want %q", i+1, w.Status, pi.StatusCancelled)
		}
	}

	// Step 9: release blocker; probe proves both queued tickets were removed.
	if err := blockerLease.Release(); err != nil {
		t.Fatalf("release blocker lease: %v", err)
	}
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

// TestControllerForegroundAdmissionSurfacesReleaseFailure verifies that a
// failure releasing a task's lease after its worker has already completed
// is surfaced honestly: the result still reports the worker's completed
// outcome, proving the worker ran before the release failed, while the
// separately returned error carries the internal cleanup failure. The
// worker replaces the gate's state.json with malformed JSON during
// execution, so the deferred lease Release — which reloads the gate state
// — fails after the worker has settled. The root is test-scoped, so the
// intentionally corrupt ticket state is removed by t.TempDir cleanup.
func TestControllerForegroundAdmissionSurfacesReleaseFailure(t *testing.T) {
	// A real gate rooted at a test-scoped temp dir, with capacity one.
	root := t.TempDir()
	gate, err := admission.Open(root, 1)
	if err != nil {
		t.Fatalf("open gate: %v", err)
	}

	// The worker corrupts the gate's durable state file while it runs:
	// the deferred Release then reloads the state, hits the malformed
	// JSON, and fails — after this worker already completed.
	worker := &changesMutatingWorker{mutate: func(dir string) error {
		return os.WriteFile(filepath.Join(root, "state.json"), []byte("{definitely not valid json"), 0o600)
	}}
	acceptedAt := time.Now()
	controller := New(worker, WithForegroundAdmission(gate, "run-1", acceptedAt, 5*time.Minute))

	result, runErr := controller.Run(context.Background(), validRequest("task-1"))

	// The worker ran and completed before the release failed, so the
	// result must still carry its completed outcome.
	if len(result.Workers) != 1 {
		t.Fatalf("workers = %d, want 1", len(result.Workers))
	}
	if result.Workers[0].Status != pi.StatusCompleted {
		t.Fatalf("worker status = %q, want %q", result.Workers[0].Status, pi.StatusCompleted)
	}
	if result.Status != contracts.RunCompleted {
		t.Fatalf("status = %q, want %q", result.Status, contracts.RunCompleted)
	}

	// The returned error honestly carries the internal cleanup failure:
	// it names the failed release of task 1 and the state-load/JSON
	// failure produced by reloading the corrupt state.
	if runErr == nil {
		t.Fatal("Run returned nil error; the release failure must be surfaced")
	}
	if !strings.Contains(runErr.Error(), "admission release task 1") {
		t.Fatalf("Run error = %q, want it to name the failed task 1 release", runErr.Error())
	}
	if !strings.Contains(runErr.Error(), "load admission state") {
		t.Fatalf("Run error = %q, want the state-load/JSON failure from reloading the corrupt state", runErr.Error())
	}
}

// TestControllerForegroundAdmissionStartsFreshVerificationClock verifies
// that an admitted controller gives verification its own fresh
// execution-timeout context instead of reusing the context it handed the
// worker. With one admitted task the executionContext factory must be
// called exactly twice — once when the task's lease grants execution,
// and once more after every worker has settled, for verification — and
// both calls must receive the exact configured execution timeout and the
// same parent context passed to Run. The context the verifier observes
// must be the second factory-produced fresh child context, never the
// worker's execution context: reusing the worker context or skipping a
// fresh verification budget collapses the factory to one call or hands
// the verifier the wrong context.
func TestControllerForegroundAdmissionStartsFreshVerificationClock(t *testing.T) {
	// Real gate with capacity one, one scripted worker and verifier, and
	// a controller admitted with a distinctive execution timeout.
	gate, err := admission.Open(t.TempDir(), 1)
	if err != nil {
		t.Fatalf("open gate: %v", err)
	}
	worker := newScriptedWorker()
	verifier := &scriptedVerifier{result: Verification{Argv: []string{"go", "test", "./..."}, ExitCode: 0}}
	executionTimeout := 7 * time.Minute
	controller := New(worker,
		WithForegroundAdmission(gate, "run-1", time.Now(), executionTimeout),
		WithVerifier(verifier))

	// Override executionContext with a mutex-protected recorder: every
	// call appends the parent context and duration it was given, plus
	// the fresh child context it created, before returning that child.
	var mu sync.Mutex
	var parents []context.Context
	var durations []time.Duration
	var children []context.Context
	controller.executionContext = func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		child, cancel := context.WithCancel(parent)
		mu.Lock()
		parents = append(parents, parent)
		durations = append(durations, d)
		children = append(children, child)
		mu.Unlock()
		return child, cancel
	}

	// One task plus a non-empty Verify argv, run synchronously under a
	// named parent context.
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	req := validRequest("task-1")
	req.Verify = []string{"go", "test", "./..."}

	result, runErr := controller.Run(runCtx, req)
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
	if verifier.callCount() != 1 {
		t.Fatalf("verifier call count = %d, want 1", verifier.callCount())
	}
	if result.Verification == nil {
		t.Fatal("verification result missing after a completed run with a verifier and Verify argv")
	}

	// The factory must have been called exactly twice: once for the
	// admitted worker execution, then again after workers settled, for
	// verification.
	mu.Lock()
	if len(parents) != 2 {
		mu.Unlock()
		t.Fatalf("executionContext calls = %d, want 2 (worker execution, then verification)", len(parents))
	}
	if len(durations) != 2 || len(children) != 2 {
		mu.Unlock()
		t.Fatalf("executionContext recorder lengths: parents=%d durations=%d children=%d, want 2 each",
			len(parents), len(durations), len(children))
	}
	parentsCopy := append([]context.Context(nil), parents...)
	durationsCopy := append([]time.Duration(nil), durations...)
	childrenCopy := append([]context.Context(nil), children...)
	mu.Unlock()

	// Both calls must carry the exact configured duration and the same
	// parent context passed to Run.
	for i := range durationsCopy {
		if durationsCopy[i] != executionTimeout {
			t.Fatalf("executionContext call %d duration = %v, want %v", i+1, durationsCopy[i], executionTimeout)
		}
		if parentsCopy[i] != runCtx {
			t.Fatalf("executionContext call %d parent is not the Run parent context", i+1)
		}
	}

	// The worker ran under the first fresh child context.
	workerCtxs := worker.contextsSeen()
	if len(workerCtxs) != 1 {
		t.Fatalf("worker contexts = %d, want 1", len(workerCtxs))
	}
	if workerCtxs[0] != childrenCopy[0] {
		t.Fatal("worker did not run under the first executionContext child")
	}

	// The verifier ran under the second fresh child context, not the
	// worker's context.
	verifier.mu.Lock()
	verifierCtxs := append([]context.Context(nil), verifier.ctxs...)
	verifier.mu.Unlock()
	if len(verifierCtxs) != 1 {
		t.Fatalf("verifier contexts = %d, want 1", len(verifierCtxs))
	}
	if verifierCtxs[0] != childrenCopy[1] {
		t.Fatal("verifier did not run under the second fresh executionContext child")
	}
	if verifierCtxs[0] == workerCtxs[0] {
		t.Fatal("verifier ran under the worker execution context; verification must get its own fresh context")
	}
}

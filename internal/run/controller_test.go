package run

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"pi-worker/internal/contracts"
	"pi-worker/internal/pi"
)

// scriptedWorker is a concurrency-safe scripted Worker for controller
// tests. It records every invocation and its prompt, tracks the peak
// number of concurrently running calls, and returns the scripted result
// for each prompt (a completed result by default). Optional gates
// deterministically control timing:
//
//   - gate, when set, is closed as soon as gateAt calls are concurrently
//     running, and every call blocks until it closes. This proves all
//     accepted workers overlap before any of them finishes: a controller
//     that serialized workers would deadlock on the gate.
//   - release, when set per prompt, makes that call block on the channel
//     before returning, so tests can control completion order.
//   - done, when set, receives the prompt once a call has settled and is
//     about to return, acknowledging completion so tests can sequence
//     releases deterministically.
//   - hold, when set, makes a call whose context is done block on it
//     before returning its cancelled/timed-out result, simulating a
//     worker teardown that outlives cancellation. Tests use it to prove
//     the controller waits for every started worker before returning.
//   - ignoreCtx, when set, makes a call return its scripted result even
//     when the context is done, modeling a worker that settles before
//     observing cancellation. The aggregate must still report the parent
//     context state.
//
// Every call that observes the context done returns cancelled or timed-out
// depending on the context error, matching the real worker contract.
type scriptedWorker struct {
	mu        sync.Mutex
	prompts   []string
	completed []string
	ctxs      []context.Context
	active    int
	maxActive int
	holding   int

	gate             chan struct{}
	gateAt           int
	release          map[string]chan struct{}
	hold             chan struct{}
	ignoreCtx        bool
	done             chan string
	blockedOnRelease int

	results map[string]pi.WorkerResult
}

func newScriptedWorker() *scriptedWorker {
	return &scriptedWorker{results: map[string]pi.WorkerResult{}}
}

func (w *scriptedWorker) Run(ctx context.Context, req pi.WorkerRequest) pi.WorkerResult {
	w.mu.Lock()
	w.prompts = append(w.prompts, req.Prompt)
	w.ctxs = append(w.ctxs, ctx)
	w.active++
	if w.active > w.maxActive {
		w.maxActive = w.active
	}
	if w.gate != nil && w.active == w.gateAt {
		close(w.gate)
	}
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.active--
		w.mu.Unlock()
	}()

	if req.Debug != nil {
		req.Debug.Worker(req.WorkerID).Log("phase=starting", "provider=acme", "model=m-1")
	}
	if w.gate != nil {
		<-w.gate
	}
	if ch := w.release[req.Prompt]; ch != nil {
		w.mu.Lock()
		w.blockedOnRelease++
		w.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
		}
		w.mu.Lock()
		w.blockedOnRelease--
		w.mu.Unlock()
	}
	if err := ctx.Err(); err != nil && !w.ignoreCtx {
		if w.hold != nil {
			w.mu.Lock()
			w.holding++
			w.mu.Unlock()
			<-w.hold
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return pi.WorkerResult{Model: req.Model, Status: pi.StatusTimedOut, Error: "timed out"}
		}
		return pi.WorkerResult{Model: req.Model, Status: pi.StatusCancelled, Error: "cancelled"}
	}
	w.mu.Lock()
	w.completed = append(w.completed, req.Prompt)
	w.mu.Unlock()
	if w.done != nil {
		w.done <- req.Prompt
	}
	if result, ok := w.results[req.Prompt]; ok {
		return result
	}
	return pi.WorkerResult{Model: req.Model, Status: pi.StatusCompleted, Explanation: "done:" + req.Prompt}
}

func (w *scriptedWorker) callCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.prompts)
}

func (w *scriptedWorker) promptsSeen() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.prompts...)
}

func (w *scriptedWorker) completionOrder() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.completed...)
}

func (w *scriptedWorker) peakConcurrency() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.maxActive
}

func (w *scriptedWorker) holdingCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.holding
}

func (w *scriptedWorker) blockedOnReleaseCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.blockedOnRelease
}

func (w *scriptedWorker) contextsSeen() []context.Context {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]context.Context(nil), w.ctxs...)
}

// waitHolding polls until want calls are blocked in simulated teardown.
func waitHolding(t *testing.T, w *scriptedWorker, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if w.holdingCount() >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d workers reached teardown hold", w.holdingCount(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitBlocked polls until want calls are blocked on their per-prompt
// release barrier.
func waitBlocked(t *testing.T, w *scriptedWorker, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if w.blockedOnReleaseCount() >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d workers reached the release barrier", w.blockedOnReleaseCount(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitGate polls until the gate closes, or fails after a bound.
func waitGate(t *testing.T, gate chan struct{}) {
	t.Helper()
	select {
	case <-gate:
	case <-time.After(5 * time.Second):
		t.Fatalf("gate never closed: workers were not started concurrently")
	}
}

// validRequest is the minimal accepted controller request.
func validRequest(tasks ...string) Request {
	return Request{Model: "acme/m-1", Tasks: tasks, Workspace: "/workspace"}
}

func TestControllerRejectsInvalidRequestsBeforeStartingWorkers(t *testing.T) {
	tests := []struct {
		name string
		req  Request
	}{
		{name: "zero tasks", req: validRequest()},
		{name: "four tasks", req: validRequest("a", "b", "c", "d")},
		{name: "empty model", req: Request{Tasks: []string{"a"}, Workspace: "/workspace"}},
		{name: "empty workspace", req: Request{Model: "acme/m-1", Tasks: []string{"a"}}},
		{name: "empty task", req: validRequest("a", " ")},
		{name: "empty task first", req: validRequest(" ", "b")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			worker := newScriptedWorker()
			result, err := New(worker).Run(context.Background(), test.req)
			if err == nil {
				t.Fatalf("Run accepted invalid request %#v", test.req)
			}
			if result.Status != "" || len(result.Workers) != 0 {
				t.Fatalf("invalid request returned a non-zero result: %#v", result)
			}
			if worker.callCount() != 0 {
				t.Fatalf("worker invoked %d times before validation", worker.callCount())
			}
		})
	}
}

func TestControllerRunsAllAcceptedWorkersConcurrently(t *testing.T) {
	// The gate deadlocks a serializing controller: all three calls must be
	// in flight simultaneously before any of them may return.
	worker := newScriptedWorker()
	worker.gate = make(chan struct{})
	worker.gateAt = 3
	ctx := context.Background()
	done := make(chan struct{})
	var result Result
	var err error
	go func() {
		defer close(done)
		result, err = New(worker).Run(ctx, validRequest("a", "b", "c"))
	}()
	waitGate(t, worker.gate)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("controller did not return after the gate opened")
	}
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if worker.callCount() != 3 {
		t.Fatalf("worker invoked %d times, want 3", worker.callCount())
	}
	if peak := worker.peakConcurrency(); peak != 3 {
		t.Fatalf("peak concurrency = %d, want 3", peak)
	}
	// Every worker received the same parent context, and results preserve
	// input order.
	for i, seen := range worker.contextsSeen() {
		if seen != ctx {
			t.Fatalf("worker %d received a different context", i+1)
		}
	}
	if result.Status != contracts.RunCompleted {
		t.Fatalf("status = %q, want completed", result.Status)
	}
	for i, want := range []string{"a", "b", "c"} {
		if result.Workers[i].Explanation != "done:"+want {
			t.Fatalf("worker %d = %#v, want done:%s", i+1, result.Workers[i], want)
		}
	}
}

func TestControllerPreservesInputOrderUnderReversedCompletion(t *testing.T) {
	tasks := []string{"task-1", "task-2", "task-3"}
	worker := newScriptedWorker()
	worker.gate = make(chan struct{})
	worker.gateAt = 3
	worker.release = map[string]chan struct{}{
		"task-1": make(chan struct{}),
		"task-2": make(chan struct{}),
		"task-3": make(chan struct{}),
	}
	worker.done = make(chan string, len(tasks))
	done := make(chan struct{})
	var result Result
	var err error
	go func() {
		defer close(done)
		result, err = New(worker).Run(context.Background(), validRequest(tasks...))
	}()
	waitGate(t, worker.gate)
	// Complete in reverse input order, waiting for each released worker's
	// completion acknowledgment before releasing the next: closing all
	// release channels back-to-back would leave the completion order to
	// goroutine scheduling. The result must still be in input order.
	for i := len(tasks) - 1; i >= 0; i-- {
		close(worker.release[tasks[i]])
		select {
		case got := <-worker.done:
			if got != tasks[i] {
				t.Fatalf("completion acknowledged %q, want %q", got, tasks[i])
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("worker %q never acknowledged completion", tasks[i])
		}
	}
	<-done
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := worker.completionOrder(); !equalStrings(got, []string{"task-3", "task-2", "task-1"}) {
		t.Fatalf("completion order = %v, want reversed input order", got)
	}
	for i, want := range tasks {
		if result.Workers[i].Explanation != "done:"+want {
			t.Fatalf("worker %d = %#v, want done:%s (input order lost)", i+1, result.Workers[i], want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestControllerAggregatesWorkerOutcomes(t *testing.T) {
	tests := []struct {
		name    string
		results map[string]pi.WorkerResult
		want    contracts.RunStatus
	}{
		{
			name: "all completed",
			results: map[string]pi.WorkerResult{
				"a": {Status: pi.StatusCompleted, Explanation: "ok"},
				"b": {Status: pi.StatusCompleted, Explanation: "ok"},
				"c": {Status: pi.StatusCompleted, Explanation: "ok"},
			},
			want: contracts.RunCompleted,
		},
		{
			name: "mixed with one completed",
			results: map[string]pi.WorkerResult{
				"a": {Status: pi.StatusCompleted, Explanation: "ok"},
				"b": {Status: pi.StatusFailed, Error: "boom"},
			},
			want: contracts.RunPartial,
		},
		{
			name: "none completed all failed",
			results: map[string]pi.WorkerResult{
				"a": {Status: pi.StatusFailed, Error: "boom"},
				"b": {Status: pi.StatusFailed, Error: "boom"},
			},
			want: contracts.RunFailed,
		},
		{
			name: "none completed mixed failure",
			results: map[string]pi.WorkerResult{
				"a": {Status: pi.StatusFailed, Error: "boom"},
				"b": {Status: pi.StatusUnavailable, Error: "no model"},
			},
			want: contracts.RunFailed,
		},
		{
			name: "none completed with error status",
			results: map[string]pi.WorkerResult{
				"a": {Status: pi.StatusError, Error: "protocol"},
				"b": {Status: pi.StatusFailed, Error: "boom"},
			},
			want: contracts.RunFailed,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			worker := newScriptedWorker()
			worker.results = test.results
			tasks := make([]string, 0, len(test.results))
			for task := range test.results {
				tasks = append(tasks, task)
			}
			result, err := New(worker).Run(context.Background(), validRequest(tasks...))
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if result.Status != test.want {
				t.Fatalf("status = %q, want %q", result.Status, test.want)
			}
			if len(result.Workers) != len(tasks) {
				t.Fatalf("worker count = %d, want %d", len(result.Workers), len(tasks))
			}
		})
	}
}

func TestControllerParentCancellationAggregatesCancelled(t *testing.T) {
	worker := newScriptedWorker()
	worker.gate = make(chan struct{})
	worker.gateAt = 2
	// After the start gate opens, every worker parks on its per-prompt
	// release barrier. The test cancels only once every worker is blocked,
	// so cancellation deterministically reaches them before they continue:
	// the old arrangement let workers complete before cancel arrived.
	worker.release = map[string]chan struct{}{
		"a": make(chan struct{}),
		"b": make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var result Result
	var err error
	go func() {
		defer close(done)
		result, err = New(worker).Run(ctx, validRequest("a", "b"))
	}()
	waitGate(t, worker.gate)
	waitBlocked(t, worker, 2)
	cancel()
	// The parked workers settle on the cancelled context; releasing the
	// barrier keeps them from lingering either way.
	close(worker.release["a"])
	close(worker.release["b"])
	<-done
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != contracts.RunCancelled {
		t.Fatalf("status = %q, want cancelled", result.Status)
	}
	for i, w := range result.Workers {
		if w.Status != pi.StatusCancelled {
			t.Fatalf("worker %d status = %q, want cancelled", i+1, w.Status)
		}
	}
}

func TestControllerWaitsForEveryWorkerOnCancellation(t *testing.T) {
	// Cancelling the parent must not let the controller return while a
	// worker is still tearing down: it waits for every started worker,
	// then reports the aggregate cancelled state.
	worker := newScriptedWorker()
	worker.gate = make(chan struct{})
	worker.gateAt = 3
	// Every worker parks on its release barrier after the start gate, so
	// the test cancels while all three are blocked and none can settle
	// before the cancellation reaches them.
	worker.release = map[string]chan struct{}{
		"a": make(chan struct{}),
		"b": make(chan struct{}),
		"c": make(chan struct{}),
	}
	worker.hold = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var result Result
	var err error
	go func() {
		defer close(done)
		result, err = New(worker).Run(ctx, validRequest("a", "b", "c"))
	}()
	waitGate(t, worker.gate)
	waitBlocked(t, worker, 3)
	cancel()
	// Once released, every worker observes the cancelled context and
	// enters simulated teardown; the controller must wait for all of them.
	for _, task := range []string{"a", "b", "c"} {
		close(worker.release[task])
	}
	waitHolding(t, worker, 3)
	select {
	case <-done:
		t.Fatalf("controller returned while every worker was still tearing down")
	case <-time.After(200 * time.Millisecond):
	}
	close(worker.hold)
	<-done
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != contracts.RunCancelled {
		t.Fatalf("status = %q, want cancelled", result.Status)
	}
	for i, w := range result.Workers {
		if w.Status != pi.StatusCancelled {
			t.Fatalf("worker %d status = %q, want cancelled", i+1, w.Status)
		}
	}
}

func TestControllerDeadlineExceededAggregatesTimedOut(t *testing.T) {
	// An already-expired deadline must surface as timed-out (exit 7 path)
	// for every worker and for the aggregate, without launching anything.
	worker := newScriptedWorker()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	result, err := New(worker).Run(ctx, validRequest("a", "b"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != contracts.RunTimedOut {
		t.Fatalf("status = %q, want timed-out", result.Status)
	}
	for i, w := range result.Workers {
		if w.Status != pi.StatusTimedOut {
			t.Fatalf("worker %d status = %q, want timed-out", i+1, w.Status)
		}
	}
}

func TestControllerTimeoutWhileWorkersRunningAggregatesTimedOut(t *testing.T) {
	// Workers blocked in-flight when the deadline fires must return
	// timed-out results, and the aggregate must report the run timeout.
	worker := newScriptedWorker()
	worker.release = map[string]chan struct{}{
		"a": make(chan struct{}), // never closed: workers run until the deadline
		"b": make(chan struct{}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	result, err := New(worker).Run(ctx, validRequest("a", "b"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != contracts.RunTimedOut {
		t.Fatalf("status = %q, want timed-out", result.Status)
	}
	for i, w := range result.Workers {
		if w.Status != pi.StatusTimedOut {
			t.Fatalf("worker %d status = %q, want timed-out", i+1, w.Status)
		}
	}
}

func TestControllerParentStateDominatesWorkerOutcomes(t *testing.T) {
	// Even when every worker settled completed before observing the parent
	// state, an expired deadline still aggregates timed-out and a cancelled
	// parent still aggregates cancelled: the caller's signal decides.
	worker := newScriptedWorker()
	worker.ignoreCtx = true

	deadlineCtx, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelDeadline()
	result, err := New(worker).Run(deadlineCtx, validRequest("a", "b"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != contracts.RunTimedOut {
		t.Fatalf("deadline status = %q, want timed-out", result.Status)
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err = New(worker).Run(cancelCtx, validRequest("a", "b"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != contracts.RunCancelled {
		t.Fatalf("cancel status = %q, want cancelled", result.Status)
	}
}

func TestControllerLabelsDebugLinesWithWorkerIdentity(t *testing.T) {
	// The controller passes worker identities 1..N through the shared run
	// sink: every debug line carries exactly one worker label and each of
	// the three workers appears under its own number.
	var buf bytes.Buffer
	sink := pi.NewDebugSink(&buf)
	worker := newScriptedWorker()
	result, err := New(worker).Run(context.Background(), Request{
		Model:     "acme/m-1",
		Tasks:     []string{"a", "b", "c"},
		Workspace: "/workspace",
		Debug:     sink,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != contracts.RunCompleted {
		t.Fatalf("status = %q", result.Status)
	}
	out := buf.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("debug emitted %d lines, want 3:\n%s", len(lines), out)
	}
	seen := map[string]bool{}
	for _, line := range lines {
		if !strings.HasPrefix(line, "[pi-worker +") {
			t.Fatalf("line without debug prefix: %q", line)
		}
		if n := strings.Count(line, "worker="); n != 1 {
			t.Fatalf("line %q carries %d worker labels", line, n)
		}
		for _, id := range []string{"worker=1 ", "worker=2 ", "worker=3 "} {
			if strings.Contains(line, id) {
				seen[id] = true
			}
		}
	}
	for _, id := range []string{"worker=1 ", "worker=2 ", "worker=3 "} {
		if !seen[id] {
			t.Fatalf("no debug line labeled %q:\n%s", id, out)
		}
	}
}

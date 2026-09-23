package run

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/admission"
	"github.com/arasovic/pi-worker/internal/pi"
)

// replaceRunProcesses installs a fake run-processes scanner for the
// duration of one test. The original seam is restored in cleanup.
func replaceRunProcesses(t *testing.T, fake func(runID string, since time.Time) []LeftoverProcess) {
	t.Helper()
	original := runProcesses
	runProcesses = fake
	t.Cleanup(func() { runProcesses = original })
}

func TestControllerSetsRunMarkerBeforeWorkersStart(t *testing.T) {
	t.Setenv(RunMarkerEnv, "")
	gate, err := admission.Open(t.TempDir(), 2)
	if err != nil {
		t.Fatalf("open gate: %v", err)
	}
	acceptedAt := time.Now()

	replaceRunProcesses(t, func(string, time.Time) []LeftoverProcess { return nil })

	var seen string
	worker := &funcWorker{fn: func(_ context.Context, req pi.WorkerRequest) pi.WorkerResult {
		seen = os.Getenv(RunMarkerEnv)
		return pi.WorkerResult{Model: req.Model, Status: pi.StatusCompleted}
	}}
	controller := New(worker, WithForegroundAdmission(gate, "run-1", acceptedAt, 5*time.Minute))

	if _, err := controller.Run(context.Background(), validRequest("task-1")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if seen != "run-1" {
		t.Fatalf("worker saw %q, want %q", seen, "run-1")
	}
}

func TestControllerReportsLeftoverProcesses(t *testing.T) {
	for _, status := range []string{pi.StatusCompleted, pi.StatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			t.Setenv(RunMarkerEnv, "")
			gate, err := admission.Open(t.TempDir(), 2)
			if err != nil {
				t.Fatalf("open gate: %v", err)
			}
			acceptedAt := time.Now()

			var gotID string
			var gotSince time.Time
			replaceRunProcesses(t, func(runID string, since time.Time) []LeftoverProcess {
				gotID = runID
				gotSince = since
				return []LeftoverProcess{{PID: 42, Name: "node"}}
			})

			worker := &funcWorker{fn: func(_ context.Context, req pi.WorkerRequest) pi.WorkerResult {
				return pi.WorkerResult{Model: req.Model, Status: status}
			}}
			controller := New(worker, WithForegroundAdmission(gate, "run-1", acceptedAt, 5*time.Minute))

			result, err := controller.Run(context.Background(), validRequest("task-1"))
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if gotID != "run-1" {
				t.Fatalf("runProcesses runID = %q, want %q", gotID, "run-1")
			}
			if !gotSince.Equal(acceptedAt) {
				t.Fatalf("runProcesses since = %v, want %v", gotSince, acceptedAt)
			}
			want := []LeftoverProcess{{PID: 42, Name: "node"}}
			if !reflect.DeepEqual(result.LeftoverProcesses, want) {
				t.Fatalf("LeftoverProcesses = %+v, want %+v", result.LeftoverProcesses, want)
			}
			if status == pi.StatusCompleted {
				data, err := json.Marshal(result)
				if err != nil {
					t.Fatalf("marshal result: %v", err)
				}
				if !strings.Contains(string(data), `"leftoverProcesses":[{"pid":42,"name":"node"}]`) {
					t.Fatalf("marshalled result missing leftoverProcesses: %s", data)
				}
			}
		})
	}
}

func TestControllerOmitsLeftoverProcessesWhenNoneFound(t *testing.T) {
	t.Setenv(RunMarkerEnv, "")
	gate, err := admission.Open(t.TempDir(), 2)
	if err != nil {
		t.Fatalf("open gate: %v", err)
	}
	replaceRunProcesses(t, func(string, time.Time) []LeftoverProcess { return nil })

	worker := &funcWorker{fn: func(_ context.Context, req pi.WorkerRequest) pi.WorkerResult {
		return pi.WorkerResult{Model: req.Model, Status: pi.StatusCompleted}
	}}
	controller := New(worker, WithForegroundAdmission(gate, "run-1", time.Now(), 5*time.Minute))

	result, err := controller.Run(context.Background(), validRequest("task-1"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if strings.Contains(string(data), "leftoverProcesses") {
		t.Fatalf("marshalled result unexpectedly has leftoverProcesses: %s", data)
	}
}

func TestControllerWithoutRunIDNeitherMarksNorScans(t *testing.T) {
	t.Setenv(RunMarkerEnv, "")
	called := false
	replaceRunProcesses(t, func(string, time.Time) []LeftoverProcess {
		called = true
		return nil
	})

	var seen string
	worker := &funcWorker{fn: func(_ context.Context, req pi.WorkerRequest) pi.WorkerResult {
		seen = os.Getenv(RunMarkerEnv)
		return pi.WorkerResult{Model: req.Model, Status: pi.StatusCompleted}
	}}

	if _, err := New(worker).Run(context.Background(), validRequest("task-1")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if seen != "" {
		t.Fatalf("worker saw %q, want empty", seen)
	}
	if called {
		t.Fatal("runProcesses was called without a run id")
	}
}

package cli

import (
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/background"
	"github.com/arasovic/pi-worker/internal/pi"
)

// TestRunsWorkerAnswerRunningActivity pins the one-line running-worker text
// the human table shows in the ANSWER column: the fixed `active` form with
// the latest event time, the call count, and the tool when one is known.
func TestRunsWorkerAnswerRunningActivity(t *testing.T) {
	at := time.Date(2026, 9, 26, 21, 40, 12, 0, time.UTC)

	for _, tt := range []struct {
		name     string
		activity *pi.Activity
		want     string
	}{
		{
			name:     "with a tool",
			activity: &pi.Activity{LastEventAt: at, LastTool: "bash", ToolCalls: 34},
			want:     "active 2026-09-26T21:40:12Z, 34 tool calls, last bash",
		},
		{
			name:     "without a tool",
			activity: &pi.Activity{LastEventAt: at, ToolCalls: 34},
			want:     "active 2026-09-26T21:40:12Z, 34 tool calls",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := runsWorkerAnswer(background.WorkerSnapshot{Activity: tt.activity})
			if got != tt.want {
				t.Fatalf("runsWorkerAnswer = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRunsWorkerAnswerFinishedUnchanged verifies that a worker with a result
// keeps today's answer text even when it also carries the retained activity.
func TestRunsWorkerAnswerFinishedUnchanged(t *testing.T) {
	at := time.Date(2026, 9, 26, 21, 40, 12, 0, time.UTC)
	worker := background.WorkerSnapshot{
		Activity: &pi.Activity{LastEventAt: at, LastTool: "bash", ToolCalls: 34},
		Result:   &pi.WorkerResult{Explanation: "the answer\nwrapped"},
	}
	if got := runsWorkerAnswer(worker); got != "the answer wrapped" {
		t.Fatalf("runsWorkerAnswer = %q, want the finished worker's own answer", got)
	}
}

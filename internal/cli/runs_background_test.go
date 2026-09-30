package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/arasovic/pi-worker/internal/background"
	"github.com/arasovic/pi-worker/internal/contracts"
	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/run"
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

// renderRunsSnapshotText renders one snapshot's human block into a buffer and
// returns stdout, failing the test if the render reports a non-zero code or
// writes to stderr.
func renderRunsSnapshotText(t *testing.T, snap background.Snapshot) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := renderRunsSnapshot(&stdout, &stderr, false, snap, ""); code != 0 {
		t.Fatalf("renderRunsSnapshot = %d, want 0; stderr = %q", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("renderRunsSnapshot stderr = %q, want empty", stderr.String())
	}
	return stdout.String()
}

// outcomePtr returns a pointer to an outcome, for building snapshots whose
// terminal outcome line must render.
func outcomePtr(outcome contracts.Outcome) *contracts.Outcome { return &outcome }

// lineWithPrefix returns the first line of text that begins with prefix.
func lineWithPrefix(text, prefix string) (string, bool) {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line, true
		}
	}
	return "", false
}

// timeoutRunsSnapshot is a finished, timed-out snapshot whose ANSWER column
// shows the timeout error and whose result carries a warning and a wrap-up
// report: the arrangement issue #370 is about.
func timeoutRunsSnapshot(workerID int, report string) background.Snapshot {
	outcome := contracts.OutcomeTimeout
	return background.Snapshot{
		RunID:    "20260926T222744Z-7099",
		State:    background.RunTimedOut,
		Terminal: true,
		Outcome:  &outcome,
		Workers: []background.WorkerSnapshot{{
			WorkerID: workerID,
			State:    background.WorkerTimedOut,
			Task:     run.TaskProjection{Model: "acme/m-1"},
			Result: &pi.WorkerResult{
				Status:             pi.StatusTimedOut,
				Error:              "timed out: context deadline exceeded",
				Warning:            "timeout wrap-up: stopping took 5ms; report completed after 1.6s",
				PartialExplanation: report,
			},
		}},
	}
}

// TestRenderRunsSnapshotPrintsTimedOutWarningAndReportStart requires that a
// worker whose ANSWER column shows its timeout error still gets its warning
// and the start of its wrap-up report below the table, cut and pointed at
// --json, before the outcome line.
func TestRenderRunsSnapshotPrintsTimedOutWarningAndReportStart(t *testing.T) {
	report := strings.Repeat("long report text ", 30)
	out := renderRunsSnapshotText(t, timeoutRunsSnapshot(1, report))

	const wantWarning = "worker 1 warning: timeout wrap-up: stopping took 5ms; report completed after 1.6s\n"
	if !strings.Contains(out, wantWarning) {
		t.Fatalf("stdout = %q, want the warning line %q", out, wantWarning)
	}
	reportLine, ok := lineWithPrefix(out, "worker 1 report: ")
	if !ok {
		t.Fatalf("stdout = %q, want a worker report line", out)
	}
	shown := strings.TrimPrefix(reportLine, "worker 1 report: ")
	if !strings.HasSuffix(shown, "\u2026") {
		t.Fatalf("report line = %q, want it cut with an ellipsis", reportLine)
	}
	if got := utf8.RuneCountInString(strings.TrimSuffix(shown, "\u2026")); got != runsReportStartRunes {
		t.Fatalf("report line shows %d runes before the ellipsis, want %d", got, runsReportStartRunes)
	}
	if !strings.Contains(out, "full text: pi-worker runs status 20260926T222744Z-7099 --json\n") {
		t.Fatalf("stdout = %q, want the full-text pointer with the run id", out)
	}
	if strings.Index(out, wantWarning) > strings.Index(out, "outcome=") {
		t.Fatalf("stdout = %q, want the extra block before outcome=", out)
	}
	if !strings.Contains(out, "\n\n"+wantWarning) {
		t.Fatalf("stdout = %q, want one empty line before the block", out)
	}
}

// TestRenderRunsSnapshotReportAtOrUnderLimitHasNoEllipsisOrFullText requires
// that a report at or under the rune limit is shown whole, with no ellipsis
// and no pointer at --json.
func TestRenderRunsSnapshotReportAtOrUnderLimitHasNoEllipsisOrFullText(t *testing.T) {
	for _, tt := range []struct {
		name   string
		report string
	}{
		{name: "shorter than the limit", report: "a brief wrap-up report"},
		{name: "exactly the limit", report: strings.Repeat("a", runsReportStartRunes)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out := renderRunsSnapshotText(t, timeoutRunsSnapshot(1, tt.report))
			reportLine, ok := lineWithPrefix(out, "worker 1 report: ")
			if !ok {
				t.Fatalf("stdout = %q, want a report line", out)
			}
			if strings.HasSuffix(reportLine, "\u2026") {
				t.Fatalf("report line = %q, want no ellipsis at or under the limit", reportLine)
			}
			if strings.Contains(out, "full text:") {
				t.Fatalf("stdout = %q, want no full-text pointer for an uncut report", out)
			}
		})
	}
}

// TestRenderRunsSnapshotReportCutOnRuneBoundary requires that a cut never
// splits a rune: a multibyte report cut at the limit stays valid UTF-8 and
// shows exactly the limit's worth of runes before the ellipsis.
func TestRenderRunsSnapshotReportCutOnRuneBoundary(t *testing.T) {
	report := strings.Repeat("\u6f22", runsReportStartRunes+20)
	out := renderRunsSnapshotText(t, timeoutRunsSnapshot(1, report))
	reportLine, ok := lineWithPrefix(out, "worker 1 report: ")
	if !ok {
		t.Fatalf("stdout = %q, want a report line", out)
	}
	shown := strings.TrimPrefix(reportLine, "worker 1 report: ")
	if !strings.HasSuffix(shown, "\u2026") {
		t.Fatalf("report line = %q, want an ellipsis", shown)
	}
	prefix := strings.TrimSuffix(shown, "\u2026")
	if !utf8.ValidString(prefix) {
		t.Fatalf("report prefix %q is not valid UTF-8; the cut split a rune", prefix)
	}
	if got := utf8.RuneCountInString(prefix); got != runsReportStartRunes {
		t.Fatalf("report prefix = %d runes, want %d", got, runsReportStartRunes)
	}
}

// TestRenderRunsSnapshotWithoutExtraLinesMatchesTableOnly requires that a
// worker with an explanation and no warning adds nothing: no empty line, no
// extra lines, just the table and the outcome line.
func TestRenderRunsSnapshotWithoutExtraLinesMatchesTableOnly(t *testing.T) {
	snap := background.Snapshot{
		RunID:    "20260926T222744Z-7099",
		State:    background.RunCompleted,
		Terminal: true,
		Outcome:  outcomePtr(contracts.OutcomeCompleted),
		Workers: []background.WorkerSnapshot{{
			WorkerID: 1,
			State:    background.WorkerCompleted,
			Task:     run.TaskProjection{Model: "acme/m-1"},
			Result:   &pi.WorkerResult{Status: pi.StatusCompleted, Explanation: "the final answer"},
		}},
	}
	out := renderRunsSnapshotText(t, snap)
	if strings.Contains(out, "\n\n") {
		t.Fatalf("stdout = %q, want no empty line for a worker with no extra lines", out)
	}
	for _, banned := range []string{"warning:", "report:", "full text:"} {
		if strings.Contains(out, banned) {
			t.Fatalf("stdout = %q, want no %q line", out, banned)
		}
	}
	if !strings.Contains(out, "the final answer") {
		t.Fatalf("stdout = %q, want the worker's answer in the table", out)
	}
	if !strings.HasSuffix(out, "outcome=completed\n") {
		t.Fatalf("stdout = %q, want it to end with the outcome line", out)
	}
}

// TestRenderRunsSnapshotDoesNotRepeatShownPartialExplanation requires that a
// worker whose ANSWER column already shows its partial explanation gets no
// report line. Dropping that condition makes this test fail.
func TestRenderRunsSnapshotDoesNotRepeatShownPartialExplanation(t *testing.T) {
	snap := background.Snapshot{
		RunID:    "20260926T222744Z-7099",
		Terminal: true,
		Workers: []background.WorkerSnapshot{{
			WorkerID: 1,
			State:    background.WorkerCompleted,
			Task:     run.TaskProjection{Model: "acme/m-1"},
			Result: &pi.WorkerResult{
				Status:             pi.StatusCompleted,
				PartialExplanation: "shown in the answer column",
			},
		}},
	}
	out := renderRunsSnapshotText(t, snap)
	if strings.Contains(out, "report:") {
		t.Fatalf("stdout = %q, want no report line when the ANSWER column already shows it", out)
	}
	if !strings.Contains(out, "shown in the answer column") {
		t.Fatalf("stdout = %q, want the partial explanation in the table", out)
	}
	if strings.Contains(out, "\n\n") {
		t.Fatalf("stdout = %q, want no empty line when no extra lines are printed", out)
	}
}

// TestRenderRunsSnapshotExtraLinesFollowWorkerOrderAndFullTextOnce requires
// that the extra lines follow worker order and that the full-text pointer is
// printed once no matter how many reports were cut.
func TestRenderRunsSnapshotExtraLinesFollowWorkerOrderAndFullTextOnce(t *testing.T) {
	snap := background.Snapshot{
		RunID: "20260926T222744Z-7099",
		Workers: []background.WorkerSnapshot{
			{
				WorkerID: 1,
				Task:     run.TaskProjection{Model: "acme/m-1"},
				Result: &pi.WorkerResult{
					Status:             pi.StatusTimedOut,
					Error:              "timed out",
					Warning:            "warning one",
					PartialExplanation: strings.Repeat("a", runsReportStartRunes+1),
				},
			},
			{
				WorkerID: 2,
				Task:     run.TaskProjection{Model: "acme/m-2"},
				Result: &pi.WorkerResult{
					Status:             pi.StatusError,
					Error:              "boom",
					PartialExplanation: strings.Repeat("b", runsReportStartRunes+1),
				},
			},
		},
	}
	out := renderRunsSnapshotText(t, snap)
	last := -1
	for _, marker := range []string{"worker 1 warning: warning one", "worker 1 report:", "worker 2 report:", "full text:"} {
		at := strings.Index(out, marker)
		if at < 0 {
			t.Fatalf("stdout = %q, want %q", out, marker)
		}
		if at < last {
			t.Fatalf("stdout = %q, want %q after the previous marker", out, marker)
		}
		last = at
	}
	if got := strings.Count(out, "full text:"); got != 1 {
		t.Fatalf("stdout = %q, want exactly one full-text line, got %d", out, got)
	}
}

// TestRenderRunsSnapshotJSONIsUnchangedByExtraLines requires that the extra
// human lines never leak into the --json document: it stays the stored
// snapshot, byte for byte.
func TestRenderRunsSnapshotJSONIsUnchangedByExtraLines(t *testing.T) {
	snap := timeoutRunsSnapshot(1, strings.Repeat("x", runsReportStartRunes+5))
	var stdout, stderr bytes.Buffer
	if code := renderRunsSnapshot(&stdout, &stderr, true, snap, ""); code != 0 {
		t.Fatalf("renderRunsSnapshot --json = %d, stderr = %q", code, stderr.String())
	}
	want, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if stdout.String() != string(want)+"\n" {
		t.Fatalf("json stdout = %q, want the stored document %q", stdout.String(), string(want))
	}
	if stderr.Len() != 0 {
		t.Fatalf("json stderr = %q, want empty", stderr.String())
	}
}

// TestRenderRunsSnapshotPrintsRunChecksBeforeOutcome requires that a finished
// run carrying its result document shows the same check lines a foreground
// run prints, between the table and the outcome line.
func TestRenderRunsSnapshotPrintsRunChecksBeforeOutcome(t *testing.T) {
	snap := background.Snapshot{
		RunID:    "20260926T222744Z-7099",
		State:    background.RunCompleted,
		Terminal: true,
		Outcome:  outcomePtr(contracts.OutcomeCompleted),
		Result: &run.Result{
			Outcome:      contracts.OutcomeCompleted,
			Verification: &run.Verification{ExitCode: 0},
			Changes:      &run.Changes{Omitted: "not a git repository"},
		},
		Workers: []background.WorkerSnapshot{{
			WorkerID: 1,
			State:    background.WorkerCompleted,
			Task:     run.TaskProjection{Model: "acme/m-1"},
			Result:   &pi.WorkerResult{Status: pi.StatusCompleted, Explanation: "the final answer"},
		}},
	}
	out := renderRunsSnapshotText(t, snap)
	const want = "verification: ok\nchanges: omitted: not a git repository\noutcome=completed\n"
	if !strings.HasSuffix(out, want) {
		t.Fatalf("stdout = %q, want it to end with %q", out, want)
	}
}

// TestRunsWaitExitsByStoredOutcome requires that a finished background run
// exits by the outcome its snapshot stores, not by one recomputed from the
// result. A controller that returns a full result together with an error is
// recorded as failed with outcome internal-error while its workers all
// completed: recomputing from those workers would say task-failed and exit
// 5 under a printed outcome=internal-error.
func TestRunsWaitExitsByStoredOutcome(t *testing.T) {
	status := contracts.RunFailed
	snap := background.Snapshot{
		RunID:    "20260926T222744Z-7099",
		State:    background.RunFailed,
		Terminal: true,
		Status:   &status,
		Outcome:  outcomePtr(contracts.OutcomeInternalError),
		Result: &run.Result{
			Status:  contracts.RunFailed,
			Outcome: contracts.OutcomeInternalError,
			Workers: []pi.WorkerResult{{Status: pi.StatusCompleted, Explanation: "the final answer"}},
		},
		Workers: []background.WorkerSnapshot{{
			WorkerID: 1,
			State:    background.WorkerCompleted,
			Task:     run.TaskProjection{Model: "acme/m-1"},
			Result:   &pi.WorkerResult{Status: pi.StatusCompleted, Explanation: "the final answer"},
		}},
	}
	var stdout, stderr bytes.Buffer
	code := runsRenderWaited(&stdout, &stderr, runsOptions{}, snap, "")
	if !strings.HasSuffix(stdout.String(), "outcome=internal-error\n") {
		t.Fatalf("stdout = %q, want it to end with outcome=internal-error", stdout.String())
	}
	if code != 9 {
		t.Fatalf("exit = %d, want 9", code)
	}
}

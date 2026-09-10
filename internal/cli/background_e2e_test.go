package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/background"
	"github.com/arasovic/pi-worker/internal/testutil/fakepi/script"
)

var (
	backgroundBinOnce sync.Once
	backgroundBinPath string
	backgroundBinErr  error
)

// piWorkerBinForBackground builds the production binary once per test run. A
// background run is executed by a fresh process of the program that dispatches
// role tokens, and the test binary is not that program.
func piWorkerBinForBackground(t *testing.T) string {
	t.Helper()
	backgroundBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "pi-worker-cli-background-bin-*")
		if err != nil {
			backgroundBinErr = err
			return
		}
		backgroundBinPath = filepath.Join(dir, "pi-worker")
		build := exec.Command("go", "build", "-o", backgroundBinPath, "github.com/arasovic/pi-worker/cmd/pi-worker")
		if out, buildErr := build.CombinedOutput(); buildErr != nil {
			backgroundBinErr = fmt.Errorf("build pi-worker: %v\n%s", buildErr, out)
		}
	})
	if backgroundBinErr != nil {
		t.Fatalf("%v", backgroundBinErr)
	}
	return backgroundBinPath
}

// backgroundHappyScript answers one worker with finalText.
func backgroundHappyScript(finalText string) *script.Script {
	return &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}},
		},
		"set_model": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"provider":"acme","id":"m-1"}`)}},
		},
		"prompt": {
			{Response: &script.Response{Success: true}},
			{Event: json.RawMessage(`{"type":"agent_start"}`)},
			{Event: json.RawMessage(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`)},
			{Event: json.RawMessage(`{"type":"turn_end","message":{},"toolResults":[]}`)},
			{Event: json.RawMessage(`{"type":"agent_end","messages":[],"willRetry":false}`)},
			{Event: json.RawMessage(`{"type":"agent_settled"}`)},
		},
		"get_last_assistant_text": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"text":"` + finalText + `"}`)}},
		},
	}}
}

// setupBackgroundCLI points one background run at a scratch workspace, a
// scratch state root, the built binary and the fake Pi, and returns the
// Manager that reads the same state the command wrote.
func setupBackgroundCLI(t *testing.T, finalText string) *background.Manager {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("background runs need a platform that can host a role process")
	}
	// The binary is built before the working directory moves: `go build`
	// needs to run inside the module, and the run itself must not.
	roleBin := piWorkerBinForBackground(t)
	setupFakePiScript(t, backgroundHappyScript(finalText))
	t.Chdir(t.TempDir())

	root, admissionRoot := t.TempDir(), t.TempDir()
	manager, err := background.NewManager(root, admissionRoot, 2)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	originalManager, originalPi, originalRole := newBackgroundManager, backgroundPiExecutable, backgroundRoleExecutable
	newBackgroundManager = func(string, int) (*background.Manager, error) { return manager, nil }
	backgroundPiExecutable = fakePiBin
	backgroundRoleExecutable = roleBin
	t.Cleanup(func() {
		newBackgroundManager, backgroundPiExecutable, backgroundRoleExecutable = originalManager, originalPi, originalRole
	})
	return manager
}

// TestRunBackgroundReturnsAcceptedAndTheRunFinishesAfterwards is the whole
// command in one test: `run --background` returns while the run is still
// going, and the run it started reaches its terminal state on its own,
// after the command that started it has returned.
func TestRunBackgroundReturnsAcceptedAndTheRunFinishesAfterwards(t *testing.T) {
	manager := setupBackgroundCLI(t, "background answer")

	code, stdout, stderr := runCLI(t, []string{"run", "--background", "--model", "acme/m-1", "--task", "go", "--timeout", "5m"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "accepted") {
		t.Fatalf("stdout = %q, want it to report the run as accepted", stdout)
	}

	// The identity the command printed is the one every later question
	// about this run takes, so it has to be readable from the output.
	runID := runIDFromHumanOutput(t, stdout)
	inFlight, err := manager.Status(runID)
	if err != nil {
		t.Fatalf("Status of the run the command reported: %v", err)
	}
	if inFlight.RunID != runID {
		t.Fatalf("stored run %q does not match the reported one %q", inFlight.RunID, runID)
	}

	final, err := manager.Wait(context.Background(), runID, 90*time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !final.Terminal || final.State != background.RunCompleted {
		t.Fatalf("run after the command returned: terminal=%v state=%q", final.Terminal, final.State)
	}
	if len(final.Workers) != 1 || final.Workers[0].Result == nil || final.Workers[0].Result.Explanation != "background answer" {
		t.Fatalf("workers = %+v, want one worker carrying the fake Pi answer", final.Workers)
	}
}

// TestRunBackgroundJSONIsTheAcceptedSnapshot requires that the machine format
// is exactly the run's own stored state, not a second shape invented for the
// command.
func TestRunBackgroundJSONIsTheAcceptedSnapshot(t *testing.T) {
	manager := setupBackgroundCLI(t, "background answer")

	code, stdout, stderr := runCLI(t, []string{"run", "--background", "--json", "--model", "acme/m-1", "--task", "go", "--timeout", "5m"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	document := decodeJSONObject(t, stdout)
	runID, ok := document["runId"].(string)
	if !ok || runID == "" {
		t.Fatalf("runId = %#v, want the accepted run's identity", document["runId"])
	}
	if state, _ := document["state"].(string); state != string(background.RunAccepted) {
		t.Fatalf("state = %#v, want %q", document["state"], background.RunAccepted)
	}
	if terminal, _ := document["terminal"].(bool); terminal {
		t.Fatal("terminal = true: the command must return while the run is still going")
	}
	if workers, _ := document["workers"].([]any); len(workers) != 1 {
		t.Fatalf("workers = %#v, want one", document["workers"])
	}

	// It is the stored snapshot, byte for byte: the same document the run's
	// own state file holds at acceptance.
	stored, err := manager.Status(runID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if stored.RunID != runID || stored.Supervisor.PID <= 0 {
		t.Fatalf("stored snapshot = %+v, want the accepted run bound to its supervisor", stored)
	}
	t.Cleanup(func() {
		if _, waitErr := manager.Wait(context.Background(), runID, 90*time.Second); waitErr != nil {
			t.Errorf("drain run %s: %v", runID, waitErr)
		}
	})
}

// TestRunBackgroundRefusedStartExitsNonZero requires that a start nobody
// accepted is reported as a failure with its reason, never as an accepted run.
func TestRunBackgroundRefusedStartExitsNonZero(t *testing.T) {
	setupBackgroundCLI(t, "never runs")

	// A worktree name in a directory that is not a repository cannot be
	// prepared, so the start is refused before any supervisor exists.
	code, stdout, stderr := runCLI(t, []string{"run", "--background", "--worktree", "nope", "--model", "acme/m-1", "--task", "go", "--timeout", "5m"}, "")
	if code == 0 {
		t.Fatalf("exit = 0 for a start that could not be made; stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "worktree") {
		t.Fatalf("stderr = %q, want it to name what could not be prepared", stderr)
	}
	if strings.Contains(stdout, "accepted") {
		t.Fatalf("stdout = %q, want no accepted run reported", stdout)
	}
}

// runIDFromHumanOutput reads the run identity out of the human line.
func runIDFromHumanOutput(t *testing.T, stdout string) string {
	t.Helper()
	fields := strings.Fields(stdout)
	if len(fields) < 2 || fields[0] != "run" {
		t.Fatalf("stdout = %q, want it to start with the run identity", stdout)
	}
	return fields[1]
}

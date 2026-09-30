//go:build darwin || linux

package background

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/testutil/fakepi/script"
)

// TestWorkerHostDebugLogUnavailableIsAWarning requires that a debug run whose
// debug file cannot be opened still runs its worker, and says so in the
// worker's warning, after any warning the worker already carried.
func TestWorkerHostDebugLogUnavailableIsAWarning(t *testing.T) {
	// A startup retry gives the result a warning of its own, so the join
	// with it is what is checked.
	cfg := happyPathScript("debug answer")
	cfg.TriggerSequences = map[string][][]script.Step{
		"get_available_models": {
			{{Response: &script.Response{Success: false, Error: "temporary catalog outage"}}},
			{{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}}},
		},
	}
	setupFakePiEnv(t, cfg)
	// A directory where the debug file belongs cannot be opened for writing.
	debugLog := filepath.Join(t.TempDir(), debugLogName)
	if err := os.Mkdir(debugLog, 0o700); err != nil {
		t.Fatalf("create directory at the debug file: %v", err)
	}

	fx := newWorkerHostFixture(t)
	req := validWorkerHostRequest()
	req.workspace = t.TempDir()
	req.piExecutable = fakePiBin(t)
	req.debugLog = debugLog
	req.debugStart = time.Now().UTC()
	sendWorkerHostRequest(t, fx, req)
	done := startWorkerHostChild(fx)

	final := readWorkerHostTerminalFrame(t, fx)
	if final.result.Status != pi.StatusCompleted || final.result.Explanation != "debug answer" {
		t.Fatalf("terminal result = %+v, want the worker completed despite its debug file", final.result)
	}
	const retry = "startup succeeded on attempt 2/3 after unavailable startup failure; "
	if !strings.HasPrefix(final.result.Warning, retry+"debug log unavailable: ") {
		t.Fatalf("warning = %q, want the retry warning joined with the debug log warning", final.result.Warning)
	}
	if out := waitWorkerHostChild(t, done, 15*time.Second); out.err != nil {
		t.Fatalf("receiveWorkerHost: %v", out.err)
	}
}

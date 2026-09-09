//go:build darwin || linux

package background

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/testutil/fakepi/script"
)

// TestWorkerHostChildExecutesSelectedWorkspaceModelThinking runs one
// handler exchange whose request selects a real workspace, an explicit
// model, an explicit thinking level, and worker identity 3: the fake Pi
// must run in exactly that workspace (its recorded cwd), answer the
// selected model catalog, apply the selected thinking level, and the
// terminal result must carry the request's model and thinking metadata.
// Nothing of the request — prompt, model, thinking, or workspace — may
// appear in the Pi process argv.
func TestWorkerHostChildExecutesSelectedWorkspaceModelThinking(t *testing.T) {
	cfg := happyPathScript("selected answer")
	cfg.TriggerSequences = map[string][][]script.Step{
		"get_state": {
			{{Response: &script.Response{Success: true, Data: json.RawMessage(`{"model":{"provider":"acme","id":"m-1"},"thinkingLevel":"medium"}`)}}},
			{{Response: &script.Response{Success: true, Data: json.RawMessage(`{"model":{"provider":"acme","id":"m-1"},"thinkingLevel":"max"}`)}}},
		},
	}
	cfg.Triggers["get_available_thinking_levels"] = []script.Step{{Response: &script.Response{Success: true, Data: json.RawMessage(`{"levels":["off","medium","max"]}`)}}}
	cfg.Triggers["set_thinking_level"] = []script.Step{{Response: &script.Response{Success: true}}}
	metaPath := filepath.Join(t.TempDir(), "fakepi-meta.json")
	setupFakePiEnv(t, cfg)
	t.Setenv("FAKEPI_META", metaPath)

	fx := newWorkerHostFixture(t)
	req := validWorkerHostRequest()
	req.workerID = 3
	req.workspace = t.TempDir()
	req.model = "acme/m-1"
	req.thinkingLevel = pi.ThinkingMax
	req.piExecutable = fakePiBin(t)
	sendWorkerHostRequest(t, fx, req)
	done := startWorkerHostChild(fx)

	// The process-start notification carries the selected worker identity.
	start := readWorkerHostFrame(t, fx)
	if start.kind != workerHostFrameProcessStart || start.workerID != 3 || start.pid <= 0 {
		t.Fatalf("process-start frame = %+v, want worker identity 3", start)
	}
	final := readWorkerHostTerminalFrame(t, fx)
	if final.result.Status != pi.StatusCompleted || final.result.Explanation != "selected answer" {
		t.Fatalf("terminal result = %+v, want completed", final.result)
	}
	if final.result.Model != "acme/m-1" {
		t.Fatalf("result model = %q, want the selected model", final.result.Model)
	}
	if final.result.RequestedThinkingLevel != pi.ThinkingMax {
		t.Fatalf("requested thinking level = %q, want the selected max", final.result.RequestedThinkingLevel)
	}
	out := waitWorkerHostChild(t, done, 15*time.Second)
	if out.err != nil {
		t.Fatalf("receiveWorkerHost: %v", out.err)
	}

	// The request log proves the run applied the selected thinking level.
	types := waitRequestLog(t, os.Getenv("FAKEPI_LOG"), 7)
	foundSetThinking := false
	for _, typ := range types {
		if typ == "set_thinking_level" {
			foundSetThinking = true
		}
	}
	if !foundSetThinking {
		t.Fatalf("request log = %v, want the selected thinking level applied", types)
	}

	// The fake Pi ran in exactly the selected workspace, and its argv
	// carries no request payload.
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read fakepi meta: %v", err)
	}
	var meta struct {
		Argv []string `json:"argv"`
		Cwd  string   `json:"cwd"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("decode fakepi meta: %v", err)
	}
	if meta.Cwd != req.workspace {
		t.Fatalf("fakepi cwd = %q, want the selected workspace %q", meta.Cwd, req.workspace)
	}
	for _, arg := range meta.Argv {
		if arg == req.prompt || arg == req.model || arg == string(req.thinkingLevel) || arg == req.workspace {
			t.Fatalf("request payload %q appears in the Pi argv: %v", arg, meta.Argv)
		}
	}
}

package pi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/testutil/fakepi/script"
)

// fakePiBin is the path of the fakepi helper binary built once per test run.
var fakePiBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "pi-worker-fakepi-bin-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create fakepi build directory: %v\n", err)
		os.Exit(1)
	}
	fakePiBin = filepath.Join(dir, "fakepi")
	build := exec.Command("go", "build", "-o", fakePiBin, "github.com/arasovic/pi-worker/internal/testutil/fakepi")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build fakepi: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// setupFakePiEnv points FAKEPI_SCRIPT and FAKEPI_LOG at a fresh script and
// request log for one test and returns the log path.
func setupFakePiEnv(t *testing.T, scriptConfig *script.Script) string {
	t.Helper()
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "script.json")
	if scriptConfig != nil {
		if scriptConfig.Triggers == nil {
			scriptConfig.Triggers = make(map[string][]script.Step)
		}
		if _, hasTrigger := scriptConfig.Triggers["get_state"]; !hasTrigger && len(scriptConfig.TriggerSequences["get_state"]) == 0 {
			scriptConfig.Triggers["get_state"] = []script.Step{{Response: &script.Response{
				Success: true,
				Data:    json.RawMessage(`{"model":{"provider":"acme","id":"m-1"},"thinkingLevel":"medium","isStreaming":false}`),
			}}}
		}
		data, err := json.Marshal(scriptConfig)
		if err != nil {
			t.Fatalf("marshal script: %v", err)
		}
		if err := os.WriteFile(scriptPath, data, 0o600); err != nil {
			t.Fatalf("write script: %v", err)
		}
	}
	logPath := filepath.Join(dir, "requests.log")
	t.Setenv("FAKEPI_SCRIPT", scriptPath)
	t.Setenv("FAKEPI_LOG", logPath)
	return logPath
}

// startScriptedPi launches fakepi with the given script over a real Process.
func startScriptedPi(t *testing.T, scriptConfig *script.Script) *Process {
	t.Helper()
	setupFakePiEnv(t, scriptConfig)
	proc, err := NewProcess(fakePiBin, t.TempDir())
	if err != nil {
		t.Fatalf("new process: %v", err)
	}
	if err := proc.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = proc.Close() })
	return proc
}

// waitRequestLog polls the fakepi request log until at least min entries are
// recorded and returns their request types in order.
func waitRequestLog(t *testing.T, path string, min int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		types := readRequestLog(path)
		if len(types) >= min {
			return types
		}
		if time.Now().After(deadline) {
			t.Fatalf("request log %s has %d entries, want %d: %v", path, len(types), min, types)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readRequestLog(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var types []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var req struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		types = append(types, req.Type)
	}
	return types
}

// sessionDirFromMeta reads the fakepi meta file and returns the
// --session-dir value from the recorded argv. It is the precise per-run
// session directory of the launched child, so cleanup assertions never
// depend on global temp-directory state that other test binaries share.
func sessionDirFromMeta(t *testing.T, metaPath string) string {
	t.Helper()
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read fakepi meta: %v", err)
	}
	var meta struct {
		Argv []string `json:"argv"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("decode fakepi meta: %v", err)
	}
	for i, arg := range meta.Argv {
		if arg == "--session-dir" && i+1 < len(meta.Argv) {
			return meta.Argv[i+1]
		}
	}
	t.Fatalf("fakepi meta argv has no --session-dir: %v", meta.Argv)
	return ""
}

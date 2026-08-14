package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/testutil/fakepi/script"
)

// newGitWorkspace creates an isolated repository in a fresh temporary
// directory with one committed file and makes it the process working
// directory for the duration of the test, restoring the original on
// cleanup. The run command reads its workspace from os.Getwd, so a
// CLI-level test that needs a specific workspace must chdir; the tree
// must also be clean before the run, or the change manifest is omitted
// and the write check skips instead of answering.
func newGitWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// Keep temporary-repository tests independent of the host user's git
	// configuration and commit-signing setup.
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("HOME", t.TempDir())
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
		}
	}
	git("init", "-q")
	git("config", "user.email", "test@pi-worker")
	git("config", "user.name", "pi-worker test")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	git("add", "file.txt")
	git("commit", "-q", "-m", "initial")
	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(original); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
	return dir
}

func TestRunJSONCarriesWritesForDeclaredRun(t *testing.T) {
	// Root writes is present exactly when the request carried a write
	// declaration: the controller tests prove the field's serialized
	// shape and the existing exact-key assertions prove a declaration-
	// free run carries none, but nothing drove the real CLI entry point
	// with --writes and asserted the root document. A clean workspace
	// yields the clean verdict — a present undeclaredCount of zero —
	// rather than a skip reason.
	newGitWorkspace(t)
	installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "done"})
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--writes", "file.txt", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	document := decodeJSONObject(t, stdout)
	assertExactJSONKeys(t, document, "changes", "schemaVersion", "status", "workers", "writes")
	if document["status"] != "completed" {
		t.Fatalf("status = %v, want completed", document["status"])
	}
	writes, ok := document["writes"].(map[string]any)
	if !ok {
		t.Fatalf("writes = %#v, want object", document["writes"])
	}
	assertExactJSONKeys(t, writes, "undeclaredCount")
	if writes["undeclaredCount"] != float64(0) {
		t.Fatalf("undeclaredCount = %v, want present 0", writes["undeclaredCount"])
	}
}

func TestRunUndeclaredWriteExitsFourWithViolationOnStderr(t *testing.T) {
	// runExitCode is unit-tested with a constructed result, but nothing
	// drove an actual run whose worker writes an undeclared path and
	// asserted the process exits 4 with the violation on stderr. The
	// stray path must appear while the run is in progress, not before
	// it: a file present before the run makes the tree dirty, the
	// manifest is then omitted, the check skips, and the test would
	// silently stop testing what it claims. The fakepi write step leaves
	// stray.txt in its working directory — the run workspace — during
	// the prompt RPC, exactly when a real worker's tools would act.
	newGitWorkspace(t)
	installRealFakePiWorker(t)
	setupFakePiScript(t, &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}},
		},
		"set_model": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"provider":"acme","id":"m-1"}`)}},
		},
		"prompt": {
			{WriteFile: "stray.txt"},
			{Response: &script.Response{Success: true}},
			{Event: json.RawMessage(`{"type":"agent_settled"}`)},
		},
		"get_last_assistant_text": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"text":"done"}`)}},
		},
	}})

	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--writes", "file.txt"}, "")
	if code != 4 {
		t.Fatalf("exit = %d, want 4; stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "pi-worker: write check failed: 1 undeclared path") {
		t.Fatalf("stderr missing the violation count: %q", stderr)
	}
	if !strings.Contains(stderr, "  stray.txt") {
		t.Fatalf("stderr missing the undeclared path: %q", stderr)
	}
}

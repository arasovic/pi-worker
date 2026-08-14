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

func TestRunJSONDeclaredEmptyWritesCarriesCleanVerdict(t *testing.T) {
	// The declared-empty verdict was only ever asserted at struct level;
	// the marshaled document a caller actually parses must carry root
	// writes with the clean verdict when the run declared --writes ""
	// and changed nothing.
	newGitWorkspace(t)
	installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "done"})
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--writes", "", "--json"}, "")
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

func TestRunDeclaredWritesNothingReportsStrayPathAndExitsFour(t *testing.T) {
	// --writes "" declares the task writes nothing: the read-only
	// declaration. A run that then writes a path must report it
	// undeclared and exit 4 exactly like any other undeclared path — the
	// declaration is a contract the check holds the run to, not a gap it
	// skips over.
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

	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--writes", ""}, "")
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

func TestRunUndeclaredWriteJSONCarriesViolation(t *testing.T) {
	// The human-mode violation is pinned; the JSON surface is not. A
	// violation in --json mode must exit 4 with the writes object in
	// the document carrying the undeclared path, so a caller parsing
	// the document sees the same verdict the human mode prints on
	// stderr. Same mechanism as the human test: the fakepi write step
	// leaves stray.txt in the workspace during the prompt RPC.
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

	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--writes", "file.txt", "--json"}, "")
	if code != 4 {
		t.Fatalf("exit = %d, want 4; stdout = %q; stderr = %q", code, stdout, stderr)
	}
	document := decodeJSONObject(t, stdout)
	assertExactJSONKeys(t, document, "changes", "schemaVersion", "status", "workers", "writes")
	if document["status"] != "completed" {
		t.Fatalf("status = %v, want completed: the violation is a policy exit, not a run outcome", document["status"])
	}
	writes, ok := document["writes"].(map[string]any)
	if !ok {
		t.Fatalf("writes = %#v, want object", document["writes"])
	}
	assertExactJSONKeys(t, writes, "undeclared", "undeclaredCount")
	undeclared, ok := writes["undeclared"].([]any)
	if !ok || len(undeclared) != 1 || undeclared[0] != "stray.txt" {
		t.Fatalf("undeclared = %#v, want exactly stray.txt", writes["undeclared"])
	}
	if writes["undeclaredCount"] != float64(1) {
		t.Fatalf("undeclaredCount = %v, want 1", writes["undeclaredCount"])
	}
}

func TestRunSkippedWriteCheckPrintsReadableReason(t *testing.T) {
	// A dirty before-state omits the manifest, so the write check
	// cannot answer and must skip with a stated reason. grep-worthy as
	// it is, no run-level test asserted the reason text a caller
	// actually reads: requireWritesTail only pins the "writes: "
	// prefix. The stdout must carry the exact reason line, telling the
	// caller the question could not be answered and why.
	dir := newGitWorkspace(t)
	if err := os.WriteFile(filepath.Join(dir, "dirt.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write dirt: %v", err)
	}
	installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "done"})
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--writes", "file.txt"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q", stderr)
	}
	const want = "worker 1: done\n" +
		"changes: omitted: dirty before-state\n" +
		"writes: skipped: change manifest unavailable\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestRunDeclaredEmptyOnDirtyBeforeStateSkipsNotExitsFour(t *testing.T) {
	// --writes "" on a workspace whose before-state was dirty pairs the
	// writes-nothing declaration with an unmeasured manifest: the
	// read-only claim cannot be proven, so the check must skip with the
	// manifest-unavailable reason and the run must exit 0, not 4. The
	// caller who declared the empty set is exactly the one most likely
	// to read a clean verdict as proof their round wrote nothing.
	dir := newGitWorkspace(t)
	if err := os.WriteFile(filepath.Join(dir, "dirt.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write dirt: %v", err)
	}
	installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "done"})
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--writes", ""}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q", stderr)
	}
	const want = "worker 1: done\n" +
		"changes: omitted: dirty before-state\n" +
		"writes: skipped: change manifest unavailable\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

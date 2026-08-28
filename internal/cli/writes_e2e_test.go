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
// CLI-level test that needs a specific workspace must chdir.
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
	assertExactJSONKeys(t, document, "changes", "outcome", "schemaVersion", "status", "workers", "writes")
	if document["status"] != "completed" {
		t.Fatalf("status = %v, want completed", document["status"])
	}
	// The outcome key must be present even on this plain successful
	// run: an absent or empty outcome must not read as "fine".
	if _, present := document["outcome"]; !present {
		t.Fatalf("document carries no outcome key: %v", document)
	}
	if document["outcome"] != "completed" {
		t.Fatalf("outcome = %v, want completed", document["outcome"])
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
	assertExactJSONKeys(t, document, "changes", "outcome", "schemaVersion", "status", "workers", "writes")
	if document["status"] != "completed" {
		t.Fatalf("status = %v, want completed", document["status"])
	}
	if document["outcome"] != "completed" {
		t.Fatalf("outcome = %v, want completed", document["outcome"])
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
	// it: a file present before the run is a before-dirty path, the
	// manifest subtracts the untouched one, the check runs clean, and
	// the test would silently stop testing what it claims. The fakepi
	// write step leaves stray.txt in its working directory — the run
	// workspace — during the prompt RPC, exactly when a real worker's
	// tools would act.
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
	assertExactJSONKeys(t, document, "changes", "outcome", "schemaVersion", "status", "workers", "writes")
	if document["status"] != "completed" {
		t.Fatalf("status = %v, want completed: the violation is a policy exit, not a run outcome", document["status"])
	}
	if document["outcome"] != "undeclared-writes" {
		t.Fatalf("outcome = %v, want undeclared-writes", document["outcome"])
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

func TestRunDirtyBeforeStatePrintsMeasuredAndChecked(t *testing.T) {
	// A dirty before-state is measured and checked: the untouched dirty
	// path subtracts out of the manifest, which prints measured-zero,
	// and the write check answers with a clean verdict. grep-worthy as
	// it is, no run-level test asserted the exact lines a caller
	// actually reads: requireWritesTail only pins the "writes: "
	// prefix. The stdout must carry the measured line and the verdict
	// line.
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
		"changes: 0 files, +0/-0\n" +
		"writes: ok\n" +
		"outcome=completed\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestRunDirtyBeforeStateJSONCarriesCleanVerdict(t *testing.T) {
	// The dirty before-state human mode is pinned; the JSON surface is
	// not. On a dirty tree the manifest is measured, the untouched dirt
	// subtracts out, and the document must carry the clean verdict — a
	// present undeclaredCount of zero and no skip reason — not the old
	// skipped form.
	dir := newGitWorkspace(t)
	if err := os.WriteFile(filepath.Join(dir, "dirt.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write dirt: %v", err)
	}
	installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "done"})
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--writes", "file.txt", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	document := decodeJSONObject(t, stdout)
	assertExactJSONKeys(t, document, "changes", "outcome", "schemaVersion", "status", "workers", "writes")
	if document["outcome"] != "completed" {
		t.Fatalf("outcome = %v, want completed", document["outcome"])
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

func TestRunDeclaredEmptyOnDirtyBeforeStateRunsCheck(t *testing.T) {
	// --writes "" declares the task writes nothing. On a workspace
	// whose before-state was dirty the manifest is still measured — the
	// untouched dirt subtracts out to zero changes — so the read-only
	// claim is actually proven against the manifest and the check
	// answers with a clean verdict instead of skipping, and the run
	// exits 0, not 4. The caller who declared the empty set is exactly
	// the one most likely to read a clean verdict as proof their round
	// wrote nothing, and on an untouched dirty tree that proof is now
	// honest.
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
		"changes: 0 files, +0/-0\n" +
		"writes: ok\n" +
		"outcome=completed\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestRunUndeclaredWriteOnDirtyBeforeStateExitsFour(t *testing.T) {
	// The case the whole dirty-before feature exists for, proven end to
	// end: a dirty before-state is measured, not omitted, so a path the
	// worker writes that no task declared appears in the manifest and
	// the check reports it undeclared with exit 4. Before this change
	// the dirty tree omitted the manifest, the check skipped with
	// "change manifest unavailable", and the run exited 0 despite the
	// stray write. The pre-existing dirt path stays untouched and
	// subtracts out of the manifest; the stray write is the one change.
	dir := newGitWorkspace(t)
	if err := os.WriteFile(filepath.Join(dir, "dirt.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write dirt: %v", err)
	}
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

	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--writes", "file.txt"}, "")
	if code != 4 {
		t.Fatalf("exit = %d, want 4; stdout = %q; stderr = %q", code, stdout, stderr)
	}
	// The measured manifest proves the check had something to decide
	// on: the untouched dirt path is gone and the stray write is the
	// one changed file, so the run reports undeclared-writes, never a
	// skip.
	const want = "worker 1 [model=acme/m-1 thinking=medium]: done\n" +
		"changes: 1 file, +1/-0\n" +
		"  stray.txt  +1/-0\n" +
		"outcome=undeclared-writes\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	if !strings.Contains(stderr, "pi-worker: write check failed: 1 undeclared path") {
		t.Fatalf("stderr missing the violation count: %q", stderr)
	}
	if !strings.Contains(stderr, "  stray.txt") {
		t.Fatalf("stderr missing the undeclared path: %q", stderr)
	}
}

func TestRunDirtyBeforeEntryPrintsClauseOnChangesLine(t *testing.T) {
	// A manifest entry whose path was already dirty before the run must
	// say so on the changes header line: its counts are measured against
	// the last commit and include the caller's own uncommitted work, so
	// the summed +added/-deleted would otherwise read inflated. The
	// fake worker modifies the pre-dirty file.txt further during the
	// run, so the entry survives subtraction and carries dirtyBefore;
	// the declared path stays clean and the run exits 0.
	dir := newGitWorkspace(t)
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	fake := installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "done"})
	fake.runHook = func() {
		if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("alpha\nbeta\n"), 0o644); err != nil {
			t.Errorf("write file during run: %v", err)
		}
	}
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--writes", "file.txt"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q", stderr)
	}
	// The committed file.txt holds "one\n", so the final two lines read
	// +2/-1 against HEAD, and the clause names the one entry whose
	// counts include pre-run work.
	const want = "worker 1: done\n" +
		"changes: 1 file, +2/-1 (1 already modified before the run)\n" +
		"  file.txt  +2/-1\n" +
		"writes: ok\n" +
		"outcome=completed\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestRunCleanBeforeStateChangesLineHasNoClause(t *testing.T) {
	// A clean before-state stays byte-for-byte what it always was: no
	// entry is dirty before the run, so the changes header line carries
	// the count and the sums with no parenthesised clause. This is the
	// output that must not move, because most runs are not dirty.
	dir := newGitWorkspace(t)
	fake := installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "done"})
	fake.runHook = func() {
		if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
			t.Errorf("write file during run: %v", err)
		}
	}
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q", stderr)
	}
	const want = "worker 1: done\n" +
		"changes: 1 file, +1/-0\n" +
		"  file.txt  +1/-0\n" +
		"outcome=completed\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestRunDirtyBeforeJSONCarriesFieldOnlyOnDirtyEntry(t *testing.T) {
	// The JSON contract of the dirty-before marking, driven through the
	// real CLI entry point: an entry whose path was dirty before the run
	// carries dirtyBefore true, an entry whose path was not carries no
	// dirtyBefore key at all, and schemaVersion stays 1 — the field is
	// additive and optional, so a decoded document must show both
	// shapes.
	dir := newGitWorkspace(t)
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	fake := installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "done"})
	fake.runHook = func() {
		if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("alpha\nbeta\n"), 0o644); err != nil {
			t.Errorf("write file during run: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new\n"), 0o644); err != nil {
			t.Errorf("write new file during run: %v", err)
		}
	}
	// Declare both changed paths so the run exits 0 and the document is
	// the clean shape this test is about; without the declaration the
	// stray new.txt would turn it into an exit-4 violation document.
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--writes", "file.txt,new.txt", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	document := decodeJSONObject(t, stdout)
	assertExactJSONKeys(t, document, "changes", "outcome", "schemaVersion", "status", "workers", "writes")
	if document["schemaVersion"] != float64(1) {
		t.Fatalf("schemaVersion = %v, want 1", document["schemaVersion"])
	}
	changes, ok := document["changes"].(map[string]any)
	if !ok {
		t.Fatalf("changes = %#v, want object", document["changes"])
	}
	assertExactJSONKeys(t, changes, "files", "totalFiles")
	if changes["totalFiles"] != float64(2) {
		t.Fatalf("totalFiles = %v, want 2", changes["totalFiles"])
	}
	sawDirty, sawClean := false, false
	for _, raw := range requireJSONArray(t, changes["files"], "changes.files") {
		entry := raw.(map[string]any)
		switch entry["path"] {
		case "file.txt":
			sawDirty = true
			assertExactJSONKeys(t, entry, "added", "deleted", "dirtyBefore", "path", "status")
			if entry["dirtyBefore"] != true {
				t.Fatalf("file.txt dirtyBefore = %v, want true", entry["dirtyBefore"])
			}
		case "new.txt":
			sawClean = true
			assertExactJSONKeys(t, entry, "added", "deleted", "path", "status")
			if _, present := entry["dirtyBefore"]; present {
				t.Fatalf("new.txt carries dirtyBefore: %v", entry["dirtyBefore"])
			}
		}
	}
	if !sawDirty || !sawClean {
		t.Fatalf("changes.files = %v, want one dirty-before entry and one clean entry", changes["files"])
	}
}

func TestRunPartialWriteDeclarationExitsTwoBeforeAnyWorkerStarts(t *testing.T) {
	// A partial write declaration is a usage error, decided in the same
	// pass that validates the rest of the argv: the run exits 2 and no
	// worker starts. The rejection names the task that declared
	// nothing, and the writes-nothing declaration is the legal way a
	// task that will not write takes part — nothing expressible is
	// lost.
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	newGitWorkspace(t)
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "a", "--writes", "file.txt", "--task", "b"}, "")
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %q", code, stderr)
	}
	if fake.callCount() != 0 {
		t.Fatalf("worker invoked %d times before the run started", fake.callCount())
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	const want = "task 2 declared no writes while another task declared: the declaration is all-or-none; declare this task's paths, or declare the empty set if it writes nothing"
	if !strings.Contains(stderr, want) {
		t.Fatalf("stderr missing the rejection naming task 2: %q", stderr)
	}
}

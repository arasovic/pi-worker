package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/arasovic/pi-worker/internal/pi"
)

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestRunDataMissingFileExitsTwoBeforeAnyWorkerStarts(t *testing.T) {
	// A --data file that cannot be read is a usage error, decided in the
	// same pass that validates the rest of the argv: the run exits 2 and
	// no worker starts. The rejection names the file, and the error
	// carries the underlying read failure unchanged.
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	missing := filepath.Join(t.TempDir(), "missing.md")
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "a", "--data", missing}, "")
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %q", code, stderr)
	}
	if fake.callCount() != 0 {
		t.Fatalf("worker invoked %d times before the run started", fake.callCount())
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "read data file") || !strings.Contains(stderr, missing) {
		t.Fatalf("stderr missing the read failure naming the file: %q", stderr)
	}
}

func TestRunDataUnreadableFileExitsTwoBeforeAnyWorkerStarts(t *testing.T) {
	// An unreadable file is the same usage error as a missing one: the
	// read happens up front, so the permission failure exits 2 before the
	// controller runs.
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not enforced the same way on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not block reads")
	}
	path := filepath.Join(t.TempDir(), "secret.log")
	writeFile(t, path, "do not read")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "a", "--data", path}, "")
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %q", code, stderr)
	}
	if fake.callCount() != 0 {
		t.Fatalf("worker invoked %d times before the run started", fake.callCount())
	}
	if !strings.Contains(stderr, "read data file") {
		t.Fatalf("stderr missing the read failure: %q", stderr)
	}
}

func TestRunDataEmptyValueExitsTwo(t *testing.T) {
	// --data "" is a usage error: unlike --writes "", it has no "carries
	// nothing" meaning, because omitting the flag already means that.
	// Whitespace-only is the same empty value.
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	for _, args := range [][]string{
		{"run", "--model", "acme/m-1", "--task", "a", "--data", ""},
		{"run", "--model", "acme/m-1", "--task", "a", "--data="},
		{"run", "--model", "acme/m-1", "--task", "a", "--data", "   "},
	} {
		code, _, stderr := runCLI(t, args, "")
		if code != 2 {
			t.Fatalf("%v: exit = %d, want 2; stderr = %q", args, code, stderr)
		}
		if fake.callCount() != 0 {
			t.Fatalf("%v: worker invoked %d times before the rejection", args, fake.callCount())
		}
		if !strings.Contains(stderr, "invalid data") {
			t.Fatalf("%v: stderr missing the invalid-data error: %q", args, stderr)
		}
	}
}

func TestRunDataEmptyElementBetweenCommasExitsTwo(t *testing.T) {
	// A comma-separated value with a trimmed-empty element — including a
	// trailing comma — is a usage error, exactly like --writes.
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	for _, value := range []string{"a.md,,b.md", "a.md,", ", a.md"} {
		code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "x", "--data", value}, "")
		if code != 2 {
			t.Fatalf("--data %q: exit = %d, want 2; stderr = %q", value, code, stderr)
		}
		if fake.callCount() != 0 {
			t.Fatalf("--data %q: worker invoked %d times before the rejection", value, fake.callCount())
		}
		if !strings.Contains(stderr, "empty element between commas") {
			t.Fatalf("--data %q: stderr missing the empty-element error: %q", value, stderr)
		}
	}
}

func TestRunDataBeforeEveryTaskRejectedWithMultipleTasks(t *testing.T) {
	// With more than one task, a --data that precedes them all is
	// ambiguous — no task can be named — and the run is rejected with the
	// remedy stated, mirroring --writes.
	path := filepath.Join(t.TempDir(), "data.md")
	writeFile(t, path, "issue body")
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--data", path, "--task", "a", "--task", "b"}, "")
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %q", code, stderr)
	}
	if fake.callCount() != 0 {
		t.Fatalf("worker invoked %d times before the rejection", fake.callCount())
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "--data must follow the --task or --task-file it declares") {
		t.Fatalf("stderr missing the ambiguity error: %q", stderr)
	}
}

func TestRunDataTwiceForOneTaskRejected(t *testing.T) {
	// At most one --data per task, mirroring --writes: a second one for
	// the same task is a usage error before any worker starts.
	first := filepath.Join(t.TempDir(), "one.md")
	second := filepath.Join(t.TempDir(), "two.md")
	writeFile(t, first, "one")
	writeFile(t, second, "two")
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "a", "--data", first, "--data", second}, "")
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %q", code, stderr)
	}
	if fake.callCount() != 0 {
		t.Fatalf("worker invoked %d times before the rejection", fake.callCount())
	}
	if !strings.Contains(stderr, "--data specified more than once for task 1") {
		t.Fatalf("stderr missing the duplicate error: %q", stderr)
	}
}

func TestRunDataBeforeSingleTaskReachesThatTask(t *testing.T) {
	// With exactly one task the ordering rule carries no information: a
	// --data placed before the --task is that task's declaration and must
	// reach it, exactly as --writes behaves.
	path := filepath.Join(t.TempDir(), "data.md")
	writeFile(t, path, "issue body")
	fake := installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "done"})
	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--data", path, "--task", "summarize"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q", stderr)
	}
	req := mustWorkerRequest(t, fake, 1)
	if !strings.HasPrefix(req.Prompt, "summarize") {
		t.Fatalf("worker prompt = %q, want it to start with the task text", req.Prompt)
	}
	if !strings.Contains(req.Prompt, "--- MATERIAL ") || !strings.Contains(req.Prompt, path) || !strings.Contains(req.Prompt, "issue body") {
		t.Fatalf("worker prompt missing the carried material: %q", req.Prompt)
	}
}

func TestRunDataWithStdinPromptReachesTheStdinTask(t *testing.T) {
	// A prompt on stdin has no --task flag for a positional --data to
	// follow, so the single-task rule is the only way the feature can be
	// used in this input mode: the declaration must bind to the stdin
	// task, exactly as --writes behaves.
	path := filepath.Join(t.TempDir(), "data.md")
	writeFile(t, path, "issue body")
	fake := installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "done"})
	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--data", path}, "do it")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	req := mustWorkerRequest(t, fake, 1)
	if !strings.HasPrefix(req.Prompt, "do it") {
		t.Fatalf("worker prompt = %q, want it to start with the stdin prompt", req.Prompt)
	}
	if !strings.Contains(req.Prompt, "issue body") {
		t.Fatalf("worker prompt missing the carried material: %q", req.Prompt)
	}
}

func TestRunDataJSONCarriesPathAndByteCountNotContent(t *testing.T) {
	// The run document reports, per worker, each carried file's path and
	// byte count — and never the content. The byte count is the length of
	// the content actually read and composed, and the path is the label
	// composed into the prompt frame.
	newGitWorkspace(t)
	path := filepath.Join(t.TempDir(), "issue-412.md")
	content := "title: API v2\n\nThe new endpoints.\n"
	writeFile(t, path, content)
	fake := installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "done"})
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "summarize", "--data", path, "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	document := decodeJSONObject(t, stdout)
	assertExactJSONKeys(t, document, "changes", "outcome", "schemaVersion", "status", "workers")
	workers := requireJSONArray(t, document["workers"], "workers")
	data := requireJSONArray(t, workers[0].(map[string]any)["data"], "workers[0].data")
	if len(data) != 1 {
		t.Fatalf("workers[0].data = %#v, want exactly one carried file", data)
	}
	entry := data[0].(map[string]any)
	assertExactJSONKeys(t, entry, "path", "byteCount")
	if entry["path"] != path {
		t.Fatalf("data.path = %v, want %q", entry["path"], path)
	}
	if entry["byteCount"] != float64(len(content)) {
		t.Fatalf("data.byteCount = %v, want %d (the content actually read and composed)", entry["byteCount"], len(content))
	}
	// Content never appears in the document: only path and byte count
	// ride in it.
	if strings.Contains(stdout, content) {
		t.Fatalf("document contains the carried content: %q", stdout)
	}
	// The worker, by contrast, received the composed prompt: the task
	// text byte-identical up front, then the framed material.
	req := mustWorkerRequest(t, fake, 1)
	if !strings.HasPrefix(req.Prompt, "summarize") {
		t.Fatalf("worker prompt = %q, want it to start with the task text", req.Prompt)
	}
	if !strings.Contains(req.Prompt, "--- MATERIAL ") || !strings.Contains(req.Prompt, path) || !strings.Contains(req.Prompt, content) {
		t.Fatalf("worker prompt missing the framed material: %q", req.Prompt)
	}
}

func TestRunDataSeveralFilesPerTask(t *testing.T) {
	// One --data per task, comma-separated value: several files per task
	// are allowed, one section each in declaration order, and the
	// document carries one entry per file with its own byte count.
	newGitWorkspace(t)
	dir := t.TempDir()
	first := filepath.Join(dir, "a.md")
	second := filepath.Join(dir, "b.md")
	writeFile(t, first, "aaa")
	writeFile(t, second, "bbbb")
	fake := installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "done"})
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "t", "--data", first + "," + second, "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	document := decodeJSONObject(t, stdout)
	workers := requireJSONArray(t, document["workers"], "workers")
	data := requireJSONArray(t, workers[0].(map[string]any)["data"], "workers[0].data")
	if len(data) != 2 {
		t.Fatalf("workers[0].data = %#v, want two carried files", data)
	}
	firstEntry := data[0].(map[string]any)
	secondEntry := data[1].(map[string]any)
	if firstEntry["path"] != first || firstEntry["byteCount"] != float64(3) {
		t.Fatalf("data[0] = %#v, want %q with byteCount 3", firstEntry, first)
	}
	if secondEntry["path"] != second || secondEntry["byteCount"] != float64(4) {
		t.Fatalf("data[1] = %#v, want %q with byteCount 4", secondEntry, second)
	}
	req := mustWorkerRequest(t, fake, 1)
	if strings.Index(req.Prompt, first) > strings.Index(req.Prompt, second) || !strings.Contains(req.Prompt, "--- END MATERIAL ") {
		t.Fatalf("worker prompt section order or delimiters wrong: %q", req.Prompt)
	}
}

func TestRunDataSeveralTasksEachWithOwnFiles(t *testing.T) {
	// Several tasks, each with its own comma-separated --data: every
	// worker's prompt carries only its own files, every worker's result
	// records only its own files, and the whole run shares one frame
	// token.
	newGitWorkspace(t)
	dir := t.TempDir()
	a1 := filepath.Join(dir, "a1.md")
	a2 := filepath.Join(dir, "a2.md")
	b1 := filepath.Join(dir, "b1.md")
	writeFile(t, a1, "one")
	writeFile(t, a2, "two")
	writeFile(t, b1, "three")
	fake := installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "done"})
	code, stdout, _ := runCLI(t, []string{
		"run", "--model", "acme/m-1",
		"--task", "first", "--data", a1 + "," + a2,
		"--task", "second", "--data", b1,
		"--json",
	}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	document := decodeJSONObject(t, stdout)
	workers := requireJSONArray(t, document["workers"], "workers")
	firstData := requireJSONArray(t, workers[0].(map[string]any)["data"], "workers[0].data")
	secondData := requireJSONArray(t, workers[1].(map[string]any)["data"], "workers[1].data")
	if len(firstData) != 2 || len(secondData) != 1 {
		t.Fatalf("data lengths = %d and %d, want 2 and 1", len(firstData), len(secondData))
	}
	if firstData[0].(map[string]any)["path"] != a1 || firstData[1].(map[string]any)["path"] != a2 {
		t.Fatalf("workers[0].data = %#v, want %q then %q", firstData, a1, a2)
	}
	if secondData[0].(map[string]any)["path"] != b1 {
		t.Fatalf("workers[1].data = %#v, want %q", secondData, b1)
	}
	firstReq := mustWorkerRequest(t, fake, 1)
	secondReq := mustWorkerRequest(t, fake, 2)
	if !strings.Contains(firstReq.Prompt, "--- MATERIAL ") || !strings.Contains(firstReq.Prompt, a1) || !strings.Contains(firstReq.Prompt, a2) || strings.Contains(firstReq.Prompt, b1) {
		t.Fatalf("worker 1 prompt carries the wrong files: %q", firstReq.Prompt)
	}
	if !strings.Contains(secondReq.Prompt, "--- MATERIAL ") || !strings.Contains(secondReq.Prompt, b1) || strings.Contains(secondReq.Prompt, a1) || strings.Contains(secondReq.Prompt, a2) {
		t.Fatalf("worker 2 prompt carries the wrong files: %q", secondReq.Prompt)
	}
	// One per-run token shared by every section of every task.
	token := strings.Split(strings.Split(firstReq.Prompt, "--- MATERIAL ")[1], ":")[0]
	if !strings.Contains(secondReq.Prompt, "--- MATERIAL "+token+": ") {
		t.Fatalf("worker 2 prompt does not share worker 1's frame token: %q", secondReq.Prompt)
	}
}

func TestRunDataAbsolutePathAccepted(t *testing.T) {
	// Absolute paths are allowed: --data reads a file rather than
	// declaring one, and the material usually sits in a temp directory
	// outside the workspace. The path is reported as composed.
	newGitWorkspace(t)
	path := filepath.Join(t.TempDir(), "spec.md")
	writeFile(t, path, "spec body")
	fake := installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "done"})
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "t", "--data", path, "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	document := decodeJSONObject(t, stdout)
	workers := requireJSONArray(t, document["workers"], "workers")
	data := requireJSONArray(t, workers[0].(map[string]any)["data"], "workers[0].data")
	if len(data) != 1 || data[0].(map[string]any)["path"] != path {
		t.Fatalf("workers[0].data = %#v, want the absolute path %q", data, path)
	}
	req := mustWorkerRequest(t, fake, 1)
	if !strings.Contains(req.Prompt, path) {
		t.Fatalf("worker prompt missing the absolute path label: %q", req.Prompt)
	}
}

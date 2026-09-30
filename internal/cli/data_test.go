package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/arasovic/pi-worker/internal/run"
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
	missing := filepath.Join(t.TempDir(), "missing.md")
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "a", "--data", missing}, "")
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %q", code, stderr)
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
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "a", "--data", path}, "")
	if code != 2 || stdout != "" {
		t.Fatalf("exit = %d, stdout = %q, want 2 and empty; stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "read data file") {
		t.Fatalf("stderr missing the read failure: %q", stderr)
	}
}

func TestRunDataEmptyValueExitsTwo(t *testing.T) {
	// --data "" is a usage error: unlike --writes "", it has no "carries
	// nothing" meaning, because omitting the flag already means that.
	// Whitespace-only is the same empty value.
	for _, args := range [][]string{
		{"run", "--model", "acme/m-1", "--task", "a", "--data", ""},
		{"run", "--model", "acme/m-1", "--task", "a", "--data="},
		{"run", "--model", "acme/m-1", "--task", "a", "--data", "   "},
	} {
		code, stdout, stderr := runCLI(t, args, "")
		if code != 2 || stdout != "" {
			t.Fatalf("%v: exit = %d, stdout = %q, want 2 and empty; stderr = %q", args, code, stdout, stderr)
		}
		if !strings.Contains(stderr, "invalid data") {
			t.Fatalf("%v: stderr missing the invalid-data error: %q", args, stderr)
		}
	}
}

func TestRunDataEmptyElementBetweenCommasExitsTwo(t *testing.T) {
	// A comma-separated value with a trimmed-empty element — including a
	// trailing comma — is a usage error, exactly like --writes.
	for _, value := range []string{"a.md,,b.md", "a.md,", ", a.md"} {
		code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "x", "--data", value}, "")
		if code != 2 || stdout != "" {
			t.Fatalf("--data %q: exit = %d, stdout = %q, want 2 and empty; stderr = %q", value, code, stdout, stderr)
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
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--data", path, "--task", "a", "--task", "b"}, "")
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %q", code, stderr)
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
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "a", "--data", first, "--data", second}, "")
	if code != 2 || stdout != "" {
		t.Fatalf("exit = %d, stdout = %q, want 2 and empty; stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "--data specified more than once for task 1") {
		t.Fatalf("stderr missing the duplicate error: %q", stderr)
	}
}

// requireTaskMaterial fails unless task keeps prompt byte-identical and
// carries exactly want, in order, content as read. The controller composes
// the MATERIAL frame from these; the run package pins that composition.
func requireTaskMaterial(t *testing.T, task run.Task, prompt string, want ...run.DataFile) {
	t.Helper()
	if task.Prompt != prompt {
		t.Fatalf("task prompt = %q, want the task text %q unchanged", task.Prompt, prompt)
	}
	if !reflect.DeepEqual(task.Data, want) {
		t.Fatalf("task material = %#v, want %#v", task.Data, want)
	}
}

func TestRunDataBeforeSingleTaskReachesThatTask(t *testing.T) {
	// With exactly one task the ordering rule carries no information: a
	// --data placed before the --task is that task's declaration and must
	// reach it, exactly as --writes behaves.
	path := filepath.Join(t.TempDir(), "data.md")
	writeFile(t, path, "issue body")
	_, tasks := mustResolveRun(t, []string{"--model", "acme/m-1", "--data", path, "--task", "summarize"}, "")
	if len(tasks) != 1 {
		t.Fatalf("tasks = %#v, want one", tasks)
	}
	requireTaskMaterial(t, tasks[0], "summarize", run.DataFile{Path: path, Content: []byte("issue body")})
}

func TestRunDataWithStdinPromptReachesTheStdinTask(t *testing.T) {
	// A prompt on stdin has no --task flag for a positional --data to
	// follow, so the single-task rule is the only way the feature can be
	// used in this input mode: the declaration must bind to the stdin
	// task, exactly as --writes behaves.
	path := filepath.Join(t.TempDir(), "data.md")
	writeFile(t, path, "issue body")
	_, tasks := mustResolveRun(t, []string{"--model", "acme/m-1", "--data", path}, "do it")
	if len(tasks) != 1 {
		t.Fatalf("tasks = %#v, want one", tasks)
	}
	requireTaskMaterial(t, tasks[0], "do it", run.DataFile{Path: path, Content: []byte("issue body")})
}

func TestRunDataJSONCarriesPathAndByteCountNotContent(t *testing.T) {
	// The run document reports, per worker, each carried file's path,
	// byte count, and SHA-256 — and never the content. The byte count is
	// the length of the content actually read and composed, the hash is
	// that same content's digest, and the path is the label composed
	// into the prompt frame.
	newGitWorkspace(t)
	path := filepath.Join(t.TempDir(), "issue-412.md")
	content := "title: API v2\n\nThe new endpoints.\n"
	writeFile(t, path, content)
	args := []string{"run", "--model", "acme/m-1", "--task", "summarize", "--data", path, "--json"}
	// The worker, by contrast, receives the task text byte-identical with
	// the material read up front.
	_, tasks := mustResolveRun(t, args[1:], "")
	requireTaskMaterial(t, tasks[0], "summarize", run.DataFile{Path: path, Content: []byte(content)})

	useFakePi(t, backgroundHappyScript("done"))
	code, stdout, stderr := runCLI(t, args, "")
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
	assertExactJSONKeys(t, entry, "path", "byteCount", "sha256")
	if entry["path"] != path {
		t.Fatalf("data.path = %v, want %q", entry["path"], path)
	}
	if entry["byteCount"] != float64(len(content)) {
		t.Fatalf("data.byteCount = %v, want %d (the content actually read and composed)", entry["byteCount"], len(content))
	}
	sum := sha256.Sum256([]byte(content))
	if entry["sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("data.sha256 = %v, want %s (SHA-256 of the content as read)", entry["sha256"], hex.EncodeToString(sum[:]))
	}
	// Content never appears in the document: only path, byte count, and
	// hash ride in it.
	if strings.Contains(stdout, content) {
		t.Fatalf("document contains the carried content: %q", stdout)
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
	args := []string{"run", "--model", "acme/m-1", "--task", "t", "--data", first + "," + second, "--json"}
	_, tasks := mustResolveRun(t, args[1:], "")
	requireTaskMaterial(t, tasks[0], "t", run.DataFile{Path: first, Content: []byte("aaa")}, run.DataFile{Path: second, Content: []byte("bbbb")})

	useFakePi(t, backgroundHappyScript("done"))
	code, stdout, stderr := runCLI(t, args, "")
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
}

func TestRunDataSeveralTasksEachWithOwnFiles(t *testing.T) {
	// Several tasks, each with its own comma-separated --data: every
	// task carries only its own files and every worker's result records
	// only its own files. The shared per-run frame token is the
	// controller's, pinned in the run package.
	newGitWorkspace(t)
	dir := t.TempDir()
	a1 := filepath.Join(dir, "a1.md")
	a2 := filepath.Join(dir, "a2.md")
	b1 := filepath.Join(dir, "b1.md")
	writeFile(t, a1, "one")
	writeFile(t, a2, "two")
	writeFile(t, b1, "three")
	args := []string{
		"run", "--model", "acme/m-1",
		"--task", "first", "--data", a1 + "," + a2,
		"--task", "second", "--data", b1,
		"--json",
	}
	_, tasks := mustResolveRun(t, args[1:], "")
	if len(tasks) != 2 {
		t.Fatalf("tasks = %#v, want two", tasks)
	}
	requireTaskMaterial(t, tasks[0], "first", run.DataFile{Path: a1, Content: []byte("one")}, run.DataFile{Path: a2, Content: []byte("two")})
	requireTaskMaterial(t, tasks[1], "second", run.DataFile{Path: b1, Content: []byte("three")})

	useFakePi(t, backgroundHappyScript("done"))
	code, stdout, _ := runCLI(t, args, "")
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
}

func TestRunDataAbsolutePathAccepted(t *testing.T) {
	// Absolute paths are allowed: --data reads a file rather than
	// declaring one, and the material usually sits in a temp directory
	// outside the workspace. The path is reported as composed.
	newGitWorkspace(t)
	path := filepath.Join(t.TempDir(), "spec.md")
	writeFile(t, path, "spec body")
	args := []string{"run", "--model", "acme/m-1", "--task", "t", "--data", path, "--json"}
	_, tasks := mustResolveRun(t, args[1:], "")
	requireTaskMaterial(t, tasks[0], "t", run.DataFile{Path: path, Content: []byte("spec body")})

	useFakePi(t, backgroundHappyScript("done"))
	code, stdout, stderr := runCLI(t, args, "")
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	document := decodeJSONObject(t, stdout)
	workers := requireJSONArray(t, document["workers"], "workers")
	data := requireJSONArray(t, workers[0].(map[string]any)["data"], "workers[0].data")
	if len(data) != 1 || data[0].(map[string]any)["path"] != path {
		t.Fatalf("workers[0].data = %#v, want the absolute path %q", data, path)
	}
}

func TestRunDataJSONSha256DistinguishesSameLengthFiles(t *testing.T) {
	// The --json document carries a sha256 per carried file, so two files
	// that a byte count cannot tell apart report different hashes, and
	// the document still carries no content and no material frame
	// anywhere.
	newGitWorkspace(t)
	dir := t.TempDir()
	first := filepath.Join(dir, "a.md")
	second := filepath.Join(dir, "b.md")
	writeFile(t, first, "aaaa")
	writeFile(t, second, "bbbb")
	args := []string{"run", "--model", "acme/m-1", "--task", "t", "--data", first + "," + second, "--json"}
	// The worker receives both files' material.
	_, tasks := mustResolveRun(t, args[1:], "")
	requireTaskMaterial(t, tasks[0], "t", run.DataFile{Path: first, Content: []byte("aaaa")}, run.DataFile{Path: second, Content: []byte("bbbb")})

	useFakePi(t, backgroundHappyScript("done"))
	code, stdout, stderr := runCLI(t, args, "")
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
	assertExactJSONKeys(t, firstEntry, "path", "byteCount", "sha256")
	assertExactJSONKeys(t, secondEntry, "path", "byteCount", "sha256")
	if firstEntry["byteCount"] != secondEntry["byteCount"] {
		t.Fatalf("byteCounts differ (%v and %v): the test needs equal lengths", firstEntry["byteCount"], secondEntry["byteCount"])
	}
	sumFirst := sha256.Sum256([]byte("aaaa"))
	sumSecond := sha256.Sum256([]byte("bbbb"))
	if firstEntry["sha256"] != hex.EncodeToString(sumFirst[:]) || secondEntry["sha256"] != hex.EncodeToString(sumSecond[:]) {
		t.Fatalf("reported hashes = %v and %v, want %x and %x", firstEntry["sha256"], secondEntry["sha256"], sumFirst, sumSecond)
	}
	if firstEntry["sha256"] == secondEntry["sha256"] {
		t.Fatalf("reported hashes are equal (%v) for different content of the same length", firstEntry["sha256"])
	}
	// Content never appears in the document: neither the bytes themselves
	// nor the material frame that carries them.
	for _, needle := range []string{"aaaa", "bbbb", "MATERIAL", "END MATERIAL"} {
		if strings.Contains(stdout, needle) {
			t.Fatalf("document contains %q; content and the material frame must never appear: %q", needle, stdout)
		}
	}
}

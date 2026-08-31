package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeListRecord writes one hand-built record file into dir: a start
// line carrying the pid, started-at, workspace, and task count the
// test chose, and — when withFinish is true — a finish line carrying
// either a result with the outcome resultOutcome or, when
// resultOutcome is empty, the error text errorText, matching the
// writer's exactly-one-of-the-two invariant. The pid is chosen by the
// test; interrupted-run-list liveness runs against the real process
// table, so unfinished records carry either this process's pid or the
// provably-dead deadPID.
func writeListRecord(t *testing.T, dir, runID string, pid int, startedAt, workspace string, tasks int, withFinish bool, resultOutcome, errorText string) string {
	t.Helper()
	start := map[string]any{
		"schemaVersion": 1,
		"event":         "start",
		"runId":         runID,
		"startedAt":     startedAt,
		"workspace":     workspace,
		"pid":           pid,
	}
	if tasks > 0 {
		list := make([]map[string]any, tasks)
		for i := range list {
			list[i] = map[string]any{"model": "acme/m-1"}
		}
		start["tasks"] = list
	}
	lines := []map[string]any{start}
	if withFinish {
		line := map[string]any{
			"schemaVersion": 1,
			"event":         "finish",
			"runId":         runID,
			"finishedAt":    startedAt,
		}
		if resultOutcome != "" {
			line["result"] = map[string]any{"schemaVersion": 1, "outcome": resultOutcome}
		} else {
			line["error"] = errorText
		}
		lines = append(lines, line)
	}
	var record strings.Builder
	for _, line := range lines {
		data, err := json.Marshal(line)
		if err != nil {
			t.Fatalf("marshal record line: %v", err)
		}
		record.Write(data)
		record.WriteByte('\n')
	}
	path := filepath.Join(dir, runID+".jsonl")
	if err := os.WriteFile(path, []byte(record.String()), 0o600); err != nil {
		t.Fatalf("write record: %v", err)
	}
	return path
}

// withRunlogDir points the runlogDir seam at dir for the duration of
// one test, exactly like the other seam installers in this package.
func withRunlogDir(t *testing.T, dir string) {
	t.Helper()
	original := runlogDir
	runlogDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { runlogDir = original })
}

// TestRunsListHumanEndToEnd drives the real runs list command against
// records written by the test — one finished, one still running (this
// test process's pid), one interrupted (a provably-dead pid) — and
// pins the human output: the header row, one line per run, the columns
// aligned by tabwriter, newest first. It also proves the command is
// read-only: the records directory holds exactly the records the test
// wrote, and no reported.json marker appeared.
func TestRunsListHumanEndToEnd(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	completedPath := writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/ws-alpha", 2, true, "completed", "")
	runningPath := writeListRecord(t, dir, "20260830T102000Z-2", os.Getpid(), "2026-08-30T10:20:00Z", "/ws-beta", 1, false, "", "")
	interruptedPath := writeListRecord(t, dir, "20260830T103000Z-3", deadPID, "2026-08-30T10:30:00Z", "/ws-gamma", 3, false, "", "")

	code, stdout, stderr := runCLI(t, []string{"runs", "list"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("runs list = (%d, %q, %q), want exit 0 with empty stderr", code, stdout, stderr)
	}
	const want = "RUN ID                STARTED                 OUTCOME        TASKS    WORKSPACE\n" +
		"20260830T103000Z-3    2026-08-30T10:30:00Z    interrupted    3        /ws-gamma\n" +
		"20260830T102000Z-2    2026-08-30T10:20:00Z    running        1        /ws-beta\n" +
		"20260830T101500Z-1    2026-08-30T10:15:00Z    completed      2        /ws-alpha\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read records dir: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("records dir entries = %d, want exactly the 3 written records; the list must write nothing", len(entries))
	}
	if _, err := os.Stat(filepath.Join(dir, "reported.json")); !os.IsNotExist(err) {
		t.Fatalf("reported.json exists after runs list; the command must not touch it: %v", err)
	}
	for _, path := range []string{completedPath, runningPath, interruptedPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("record %s vanished: %v", path, err)
		}
	}
}

// TestRunsListJSONEndToEnd drives the real runs list --json command
// against the same records and pins the document: one line, the exact
// shape, the runs newest first. The expected document is built from
// the test's own chosen values and paths, never read out of the
// command's output.
func TestRunsListJSONEndToEnd(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	p1 := writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/ws-alpha", 2, true, "completed", "")
	p2 := writeListRecord(t, dir, "20260830T102000Z-2", os.Getpid(), "2026-08-30T10:20:00Z", "/ws-beta", 1, false, "", "")
	p3 := writeListRecord(t, dir, "20260830T103000Z-3", deadPID, "2026-08-30T10:30:00Z", "/ws-gamma", 3, false, "", "")

	code, stdout, stderr := runCLI(t, []string{"runs", "list", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("runs list --json = (%d, %q, %q), want exit 0 with empty stderr", code, stdout, stderr)
	}
	if strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
		t.Fatalf("json output has multiple lines: %q", stdout)
	}
	want := fmt.Sprintf(`{"schemaVersion":1,"runs":[{"runId":"20260830T103000Z-3","startedAt":"2026-08-30T10:30:00Z","workspace":"/ws-gamma","tasks":3,"outcome":"interrupted","path":%q},{"runId":"20260830T102000Z-2","startedAt":"2026-08-30T10:20:00Z","workspace":"/ws-beta","tasks":1,"outcome":"running","path":%q},{"runId":"20260830T101500Z-1","startedAt":"2026-08-30T10:15:00Z","workspace":"/ws-alpha","tasks":2,"outcome":"completed","path":%q}]}`+"\n", p3, p2, p1)
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

// TestRunsListEmptyAndMissingDir asserts the empty case: no records
// prints exactly one line and exits 0, and the JSON document carries
// an empty array — never null — for both an empty and a missing
// records directory.
func TestRunsListEmptyAndMissingDir(t *testing.T) {
	for name, dir := range map[string]string{
		"empty":   t.TempDir(),
		"missing": filepath.Join(t.TempDir(), "missing"),
	} {
		t.Run(name, func(t *testing.T) {
			withRunlogDir(t, dir)

			code, stdout, stderr := runCLI(t, []string{"runs", "list"}, "")
			if code != 0 || stdout != "no runs recorded\n" || stderr != "" {
				t.Fatalf("runs list = (%d, %q, %q), want (0, \"no runs recorded\\n\", \"\")", code, stdout, stderr)
			}

			code, stdout, stderr = runCLI(t, []string{"runs", "list", "--json"}, "")
			if code != 0 || stdout != "{\"schemaVersion\":1,\"runs\":[]}\n" || stderr != "" {
				t.Fatalf("runs list --json = (%d, %q, %q), want the empty-array document", code, stdout, stderr)
			}
		})
	}
}

// TestRunsListUsageErrors asserts every argv mistake exits 2 with the
// usage on stderr and nothing on stdout: a missing subcommand, an
// unknown subcommand, a duplicate flag, an unknown flag, an extra
// argument, and a flag given a value.
func TestRunsListUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"runs"},
		{"runs", "prune"},
		{"runs", "list", "--json", "--json"},
		{"runs", "list", "--bogus"},
		{"runs", "list", "extra"},
		{"runs", "list", "--json=1"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, stdout, stderr := runCLI(t, args, "")
			if code != 2 || stdout != "" || !strings.Contains(stderr, "pi-worker:") {
				t.Fatalf("%v = (%d, %q, %q), want exit 2 with stderr", args, code, stdout, stderr)
			}
		})
	}
}

// TestMainUsageIncludesRunsCommand asserts the usage text carries the
// new line.
func TestMainUsageIncludesRunsCommand(t *testing.T) {
	code, stdout, stderr := runCLI(t, []string{}, "")
	if code != 2 || stdout != "" {
		t.Fatalf("empty argv = (%d, %q)", code, stdout)
	}
	if !strings.Contains(stderr, "pi-worker runs list [--json]") {
		t.Fatalf("usage missing runs command: %q", stderr)
	}
}

// TestRunsListWiredIntoMainWithContext asserts the second dispatch
// switch — mainWithContext, the seam the cancellation tests drive —
// routes runs the same way Main does.
func TestRunsListWiredIntoMainWithContext(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/workspace", 1, true, "completed", "")

	code, stdout, stderr := runCLIWithContext(t, context.Background(), []string{"runs", "list", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("mainWithContext runs list = (%d, %q, %q)", code, stdout, stderr)
	}
	var document struct {
		SchemaVersion int `json:"schemaVersion"`
		Runs          []struct {
			RunID string `json:"runId"`
		} `json:"runs"`
	}
	if err := json.Unmarshal([]byte(stdout), &document); err != nil {
		t.Fatalf("decode document %q: %v", stdout, err)
	}
	if document.SchemaVersion != 1 || len(document.Runs) != 1 || document.Runs[0].RunID != "20260830T101500Z-1" {
		t.Fatalf("document = %#v", document)
	}
}

// TestRunsListResolverAndReadFailuresExit9 asserts both failure
// classes exit 9: a records directory that cannot be resolved and one
// that exists but cannot be read.
func TestRunsListResolverAndReadFailuresExit9(t *testing.T) {
	t.Run("resolver failure", func(t *testing.T) {
		original := runlogDir
		runlogDir = func() (string, error) { return "", fmt.Errorf("config unavailable") }
		t.Cleanup(func() { runlogDir = original })

		code, stdout, stderr := runCLI(t, []string{"runs", "list"}, "")
		if code != 9 || stdout != "" || !strings.Contains(stderr, "determine records directory") {
			t.Fatalf("resolver failure = (%d, %q, %q), want exit 9", code, stdout, stderr)
		}
	})

	t.Run("unreadable records directory", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("permission bits are not enforced on Windows")
		}
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o000); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o700) })
		withRunlogDir(t, dir)

		code, stdout, stderr := runCLI(t, []string{"runs", "list"}, "")
		if code != 9 || stdout != "" || !strings.Contains(stderr, "list runs") {
			t.Fatalf("unreadable dir = (%d, %q, %q), want exit 9", code, stdout, stderr)
		}
	})
}

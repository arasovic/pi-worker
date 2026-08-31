package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/arasovic/pi-worker/internal/runlog"
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

// withStdinIsTerminal scripts the terminal seam for the duration of
// one test: the command receives stdin as an io.Reader that cannot be
// asked whether it is a terminal, so the test answers the question the
// reader cannot, exactly like the other seam installers.
func withStdinIsTerminal(t *testing.T, value bool) {
	t.Helper()
	original := stdinIsTerminal
	stdinIsTerminal = func() bool { return value }
	t.Cleanup(func() { stdinIsTerminal = original })
}

// failingReader is the unreadable-stdin script: every Read fails, so
// the prompt counts it as no answer.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

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
// runs lines, the read-only list and the prune with its flags.
func TestMainUsageIncludesRunsCommand(t *testing.T) {
	code, stdout, stderr := runCLI(t, []string{}, "")
	if code != 2 || stdout != "" {
		t.Fatalf("empty argv = (%d, %q)", code, stdout)
	}
	if !strings.Contains(stderr, "pi-worker runs list [--json]") {
		t.Fatalf("usage missing runs command: %q", stderr)
	}
	if !strings.Contains(stderr, "pi-worker runs prune --keep <n> [--yes] [--json]") {
		t.Fatalf("usage missing runs prune command: %q", stderr)
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

// TestRunsPruneKeepsNewestNAndDeletesTheRest asserts the core prune
// rule: the newest --keep entries survive whatever their outcome, and
// every later record — any non-running outcome — is deleted, reported
// oldest first.
func TestRunsPruneKeepsNewestNAndDeletesTheRest(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	oldPath := writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/ws-a", 1, true, "completed", "")
	midPath := writeListRecord(t, dir, "20260830T102000Z-2", deadPID, "2026-08-30T10:20:00Z", "/ws-b", 1, true, "error", "boom")
	newestPath := writeListRecord(t, dir, "20260830T103000Z-3", deadPID, "2026-08-30T10:30:00Z", "/ws-c", 1, true, "completed", "")

	code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "1", "--yes"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("runs prune = (%d, %q, %q), want exit 0 with empty stderr", code, stdout, stderr)
	}
	const want = "deleted 20260830T101500Z-1\ndeleted 20260830T102000Z-2\nkept 1 newest\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	for _, gone := range []string{oldPath, midPath} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("record %s still exists after prune: %v", gone, err)
		}
	}
	if _, err := os.Stat(newestPath); err != nil {
		t.Fatalf("the kept newest record %s vanished: %v", newestPath, err)
	}
}

// TestRunsPruneSparesRunningCandidateOlderThanCutoff asserts the rule
// that matters most: a candidate whose run is still running is never
// deleted, however far below the cutoff it sits. Here the newest
// record is itself a running run (kept by the --keep rule, so not a
// candidate and not reported as still running), and an older running
// run after the cutoff is spared and reported separately, while the
// completed record between them goes.
func TestRunsPruneSparesRunningCandidateOlderThanCutoff(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	newestRunning := writeListRecord(t, dir, "20260830T103000Z-3", os.Getpid(), "2026-08-30T10:30:00Z", "/ws-c", 1, false, "", "")
	completedPath := writeListRecord(t, dir, "20260830T102000Z-2", deadPID, "2026-08-30T10:20:00Z", "/ws-b", 1, true, "completed", "")
	olderRunning := writeListRecord(t, dir, "20260830T101500Z-1", os.Getpid(), "2026-08-30T10:15:00Z", "/ws-a", 1, false, "", "")

	code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "1", "--yes"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("runs prune = (%d, %q, %q), want exit 0 with empty stderr", code, stdout, stderr)
	}
	const want = "deleted 20260830T102000Z-2\nkept 1 newest, 1 still running\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	if _, err := os.Stat(completedPath); !os.IsNotExist(err) {
		t.Fatalf("completed record %s still exists: %v", completedPath, err)
	}
	for _, spared := range []string{newestRunning, olderRunning} {
		if _, err := os.Stat(spared); err != nil {
			t.Fatalf("running record %s was deleted: %v", spared, err)
		}
	}
}

// TestRunsPruneKeepZeroDeletesEverythingExceptRunning asserts --keep 0
// means "keep none by age", not "empty the directory": completed,
// interrupted, and unknown records all go — an unreadable record is
// exactly the junk the command exists to clear — while the running
// record is still spared.
func TestRunsPruneKeepZeroDeletesEverythingExceptRunning(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	runningPath := writeListRecord(t, dir, "20260830T103000Z-3", os.Getpid(), "2026-08-30T10:30:00Z", "/ws-c", 1, false, "", "")
	completedPath := writeListRecord(t, dir, "20260830T102000Z-2", deadPID, "2026-08-30T10:20:00Z", "/ws-b", 1, true, "completed", "")
	interruptedPath := writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/ws-a", 1, false, "", "")
	unknownPath := filepath.Join(dir, "20260830T101000Z-4.jsonl")
	if err := os.WriteFile(unknownPath, []byte("not json at all\n"), 0o600); err != nil {
		t.Fatalf("write unknown record: %v", err)
	}

	code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0", "--yes"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("runs prune = (%d, %q, %q), want exit 0 with empty stderr", code, stdout, stderr)
	}
	const want = "deleted 20260830T101000Z-4\ndeleted 20260830T101500Z-1\ndeleted 20260830T102000Z-2\nkept 0 newest, 1 still running\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	for _, gone := range []string{completedPath, interruptedPath, unknownPath} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("record %s still exists after prune: %v", gone, err)
		}
	}
	if _, err := os.Stat(runningPath); err != nil {
		t.Fatalf("running record %s was deleted: %v", runningPath, err)
	}
}

// TestRunsPruneYesDeletesWithoutPromptOrStdinRead asserts --yes skips
// the whole asking path: stderr stays empty — the table and the
// question are never rendered — and the stdin reader is never touched,
// whatever the terminal seam says. The stdin payload is deliberately a
// declining "n\n": only a command that reads it could change its mind.
func TestRunsPruneYesDeletesWithoutPromptOrStdinRead(t *testing.T) {
	for name, terminal := range map[string]bool{
		"terminal":     true,
		"not terminal": false,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			withRunlogDir(t, dir)
			path := writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/ws-a", 1, true, "completed", "")
			withStdinIsTerminal(t, terminal)

			code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0", "--yes"}, "n\n")
			if code != 0 || stderr != "" {
				t.Fatalf("runs prune --yes = (%d, %q, %q), want exit 0 with empty stderr", code, stdout, stderr)
			}
			if want := "deleted 20260830T101500Z-1\nkept 0 newest\n"; stdout != want {
				t.Fatalf("stdout = %q, want %q", stdout, want)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("record %s still exists: %v", path, err)
			}
		})
	}
}

// TestRunsPrunePromptAnswers asserts the four accepted answers delete
// and every other answer deletes nothing: n, an empty line, and an EOF
// all abort with `nothing deleted` and exit 0, and the record stays.
// The exact stderr block — the table then the question — is pinned in
// every case: the prompt lists what it is about to delete before it
// asks, and it all goes to stderr.
func TestRunsPrunePromptAnswers(t *testing.T) {
	const table = "RUN ID                STARTED                 OUTCOME      TASKS    WORKSPACE\n" +
		"20260830T101500Z-1    2026-08-30T10:15:00Z    completed    1        /ws-a\n"
	const question = "delete 1 run records? [y/N] "

	for _, test := range []struct {
		name    string
		answer  string
		deletes bool
	}{
		{name: "y", answer: "y\n", deletes: true},
		{name: "uppercase Y", answer: "Y\n", deletes: true},
		{name: "yes", answer: "yes\n", deletes: true},
		{name: "yes with surrounding spaces", answer: "  YES  \n", deletes: true},
		{name: "n", answer: "n\n"},
		{name: "empty line", answer: "\n"},
		{name: "EOF", answer: ""},
		{name: "anything else", answer: "maybe\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			withRunlogDir(t, dir)
			path := writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/ws-a", 1, true, "completed", "")
			withStdinIsTerminal(t, true)

			code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0"}, test.answer)
			if code != 0 {
				t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
			}
			wantStderr := table + question
			if stderr != wantStderr {
				t.Fatalf("stderr = %q, want the table and the question %q", stderr, wantStderr)
			}
			if test.deletes {
				if want := "deleted 20260830T101500Z-1\nkept 0 newest\n"; stdout != want {
					t.Fatalf("stdout = %q, want %q", stdout, want)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("record %s still exists: %v", path, err)
				}
				return
			}
			if stdout != "nothing deleted\n" {
				t.Fatalf("stdout = %q, want \"nothing deleted\\n\"", stdout)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("record %s vanished after a declining answer: %v", path, err)
			}
		})
	}
}

// TestRunsPrunePromptListsRecordsBeforeAsking asserts the stderr block
// shows every record about to be deleted, newest first as the reader
// returned them, on the line before the question — a bare "are you
// sure?" is not consent — and that nothing about the prompt leaks to
// stdout.
func TestRunsPrunePromptListsRecordsBeforeAsking(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	oldPath := writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/ws-a", 1, true, "completed", "")
	newestPath := writeListRecord(t, dir, "20260830T103000Z-3", deadPID, "2026-08-30T10:30:00Z", "/ws-b", 1, true, "interrupted", "")
	withStdinIsTerminal(t, true)

	const wantStderr = "RUN ID                STARTED                 OUTCOME        TASKS    WORKSPACE\n" +
		"20260830T103000Z-3    2026-08-30T10:30:00Z    interrupted    1        /ws-b\n" +
		"20260830T101500Z-1    2026-08-30T10:15:00Z    completed      1        /ws-a\n" +
		"delete 2 run records? [y/N] "
	code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0"}, "n\n")
	if code != 0 || stdout != "nothing deleted\n" {
		t.Fatalf("runs prune = (%d, %q), want exit 0 and only the nothing-deleted line on stdout", code, stdout)
	}
	if stderr != wantStderr {
		t.Fatalf("stderr = %q, want %q", stderr, wantStderr)
	}
	for _, path := range []string{oldPath, newestPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("record %s vanished: %v", path, err)
		}
	}
}

// TestRunsPruneUnreadableStdinAborts asserts an unreadable stdin
// counts as no answer: nothing is deleted, `nothing deleted` is
// printed, and the exit is 0.
func TestRunsPruneUnreadableStdinAborts(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	path := writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/ws-a", 1, true, "completed", "")
	withStdinIsTerminal(t, true)

	code, stdout, stderr := runCLIReader(t, []string{"runs", "prune", "--keep", "0"}, failingReader{})
	if code != 0 || stdout != "nothing deleted\n" {
		t.Fatalf("runs prune = (%d, %q), want exit 0 with the nothing-deleted line", code, stdout)
	}
	if !strings.Contains(stderr, "delete 1 run records? [y/N] ") {
		t.Fatalf("stderr = %q, want the prompt before the abort", stderr)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("record %s vanished: %v", path, err)
	}
}

// TestRunsPruneRefusesWhenStdinIsNotTerminal asserts the second
// refusal row: without --yes on a non-terminal stdin, prune deletes
// nothing, prints the verbatim refusal, and exits 2 — the y answer
// waiting on the reader is never read.
func TestRunsPruneRefusesWhenStdinIsNotTerminal(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	path := writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/ws-a", 1, true, "completed", "")
	withStdinIsTerminal(t, false)

	code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0"}, "y\n")
	if code != 2 || stdout != "" {
		t.Fatalf("runs prune = (%d, %q), want exit 2 with nothing on stdout", code, stdout)
	}
	if want := "pi-worker: runs prune needs --yes when it cannot ask\n"; stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("record %s vanished: %v", path, err)
	}
}

// TestRunsPruneJSONWithoutYesRefusesEvenOnTerminal asserts the third
// refusal row: --json without --yes refuses with the same verbatim
// message and exit 2 whatever the terminal seam says — a caller
// parsing JSON must never be handed a prompt.
func TestRunsPruneJSONWithoutYesRefusesEvenOnTerminal(t *testing.T) {
	for name, terminal := range map[string]bool{
		"terminal":     true,
		"not terminal": false,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			withRunlogDir(t, dir)
			path := writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/ws-a", 1, true, "completed", "")
			withStdinIsTerminal(t, terminal)

			code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0", "--json"}, "y\n")
			if code != 2 || stdout != "" {
				t.Fatalf("runs prune --json = (%d, %q), want exit 2 with nothing on stdout", code, stdout)
			}
			if want := "pi-worker: runs prune needs --yes when it cannot ask\n"; stderr != want {
				t.Fatalf("stderr = %q, want %q", stderr, want)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("record %s vanished: %v", path, err)
			}
		})
	}
}

// TestRunsPruneKeepZeroYesSparesMarkerAndTempStages asserts the marker
// file and a left-behind .reported.json.tmp-* stage survive a --keep 0
// --yes prune unchanged: neither is a record, and the prune must not
// reintroduce the reader's own exclusions by pruning from a directory
// listing instead.
func TestRunsPruneKeepZeroYesSparesMarkerAndTempStages(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	recordPath := writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/ws-a", 1, true, "completed", "")
	markerPath := filepath.Join(dir, "reported.json")
	markerContent := "{\"schemaVersion\":1,\"watermark\":\"20260830T101500Z-1\"}\n"
	if err := os.WriteFile(markerPath, []byte(markerContent), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	tmpPath := filepath.Join(dir, ".reported.json.tmp-77")
	if err := os.WriteFile(tmpPath, []byte("stale stage"), 0o600); err != nil {
		t.Fatalf("write temp stage: %v", err)
	}

	code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0", "--yes"}, "")
	if code != 0 || stderr != "" || stdout != "deleted 20260830T101500Z-1\nkept 0 newest\n" {
		t.Fatalf("runs prune = (%d, %q, %q)", code, stdout, stderr)
	}
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Fatalf("record %s still exists: %v", recordPath, err)
	}
	data, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("marker vanished: %v", err)
	}
	if string(data) != markerContent {
		t.Fatalf("marker changed by the prune: %q, want %q", data, markerContent)
	}
	data, err = os.ReadFile(tmpPath)
	if err != nil {
		t.Fatalf("temp stage vanished: %v", err)
	}
	if string(data) != "stale stage" {
		t.Fatalf("temp stage changed by the prune: %q", data)
	}
}

// TestRunsPruneKeepZeroYesLeavesNonJsonlFilesUntouched asserts a
// non-.jsonl file in the records directory is never a candidate, so a
// --keep 0 --yes prune that clears every record leaves it byte-identical.
func TestRunsPruneKeepZeroYesLeavesNonJsonlFilesUntouched(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	recordPath := writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/ws-a", 1, true, "completed", "")
	notesPath := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notesPath, []byte("not a record\n"), 0o600); err != nil {
		t.Fatalf("write notes: %v", err)
	}

	code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0", "--yes"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("runs prune = (%d, %q, %q)", code, stdout, stderr)
	}
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Fatalf("record %s still exists: %v", recordPath, err)
	}
	data, err := os.ReadFile(notesPath)
	if err != nil {
		t.Fatalf("notes.txt vanished: %v", err)
	}
	if string(data) != "not a record\n" {
		t.Fatalf("notes.txt changed by the prune: %q", data)
	}
}

// TestRunsPruneJSONDocumentExact pins the prune --json document: the
// exact field names and shape, deleted and keptRunning as arrays of
// run ids — empty arrays, never null, whether nothing was deleted or
// no running run was spared — and keptNewest as how many the --keep
// rule kept, capped by the records that exist.
func TestRunsPruneJSONDocumentExact(t *testing.T) {
	// One fresh records directory per subtest: a prune that deleted
	// something in an earlier subtest must not change what a later
	// subtest counts.
	withRecords := func(t *testing.T) (dir string, newestPath, runningPath, oldPath string) {
		t.Helper()
		dir = t.TempDir()
		withRunlogDir(t, dir)
		newestPath = writeListRecord(t, dir, "20260830T103000Z-3", deadPID, "2026-08-30T10:30:00Z", "/ws-c", 1, true, "completed", "")
		runningPath = writeListRecord(t, dir, "20260830T102000Z-2", os.Getpid(), "2026-08-30T10:20:00Z", "/ws-b", 1, false, "", "")
		oldPath = writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/ws-a", 1, true, "completed", "")
		return dir, newestPath, runningPath, oldPath
	}

	t.Run("deleted and keptRunning", func(t *testing.T) {
		_, newestPath, runningPath, oldPath := withRecords(t)
		code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "1", "--yes", "--json"}, "")
		if code != 0 || stderr != "" {
			t.Fatalf("runs prune --json = (%d, %q, %q)", code, stdout, stderr)
		}
		want := "{\"schemaVersion\":1,\"deleted\":[\"20260830T101500Z-1\"],\"keptNewest\":1,\"keptRunning\":[\"20260830T102000Z-2\"]}\n"
		if stdout != want {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
		if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
			t.Fatalf("record %s still exists: %v", oldPath, err)
		}
		for _, kept := range []string{newestPath, runningPath} {
			if _, err := os.Stat(kept); err != nil {
				t.Fatalf("kept record %s vanished: %v", kept, err)
			}
		}
	})

	t.Run("empty arrays never null", func(t *testing.T) {
		_, newestPath, runningPath, oldPath := withRecords(t)
		code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "5", "--yes", "--json"}, "")
		if code != 0 || stderr != "" {
			t.Fatalf("runs prune --json = (%d, %q, %q)", code, stdout, stderr)
		}
		want := "{\"schemaVersion\":1,\"deleted\":[],\"keptNewest\":3,\"keptRunning\":[]}\n"
		if stdout != want {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
		for _, path := range []string{oldPath, newestPath, runningPath} {
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("record %s vanished: %v", path, err)
			}
		}
	})

	t.Run("missing records directory", func(t *testing.T) {
		withRunlogDir(t, filepath.Join(t.TempDir(), "missing"))
		code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0", "--yes", "--json"}, "")
		if code != 0 || stderr != "" {
			t.Fatalf("runs prune --json = (%d, %q, %q)", code, stdout, stderr)
		}
		want := "{\"schemaVersion\":1,\"deleted\":[],\"keptNewest\":0,\"keptRunning\":[]}\n"
		if stdout != want {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
	})
}

// TestRunsPruneDeleteFailureContinuesAndExits9 asserts a delete
// failure reports on stderr and exits 9 while every other selected
// record still goes: the delete path re-validates each path against
// the records directory, so a listed record whose path lies outside it
// is refused, and the listed record inside it is still deleted. The
// refusal is injected through the runlogList seam, exactly as the seam
// exists to be.
func TestRunsPruneDeleteFailureContinuesAndExits9(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	realPath := writeListRecord(t, dir, "20260830T103000Z-1", deadPID, "2026-08-30T10:30:00Z", "/ws-a", 1, true, "completed", "")
	outsidePath := filepath.Join(t.TempDir(), "20260830T101500Z-0.jsonl")
	if err := os.WriteFile(outsidePath, []byte("{\"schemaVersion\":1}\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	original := runlogList
	runlogList = func(string) ([]runlog.Run, error) {
		return []runlog.Run{
			{RunID: "20260830T103000Z-1", Outcome: "completed", Path: realPath},
			{RunID: "20260830T101500Z-0", Outcome: "completed", Path: outsidePath},
		}, nil
	}
	t.Cleanup(func() { runlogList = original })

	code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0", "--yes"}, "")
	if code != 9 {
		t.Fatalf("exit = %d, want 9; stdout = %q; stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "deleted 20260830T103000Z-1\n") {
		t.Fatalf("stdout = %q, want the deleted line for the record that went", stdout)
	}
	if !strings.Contains(stdout, "kept 0 newest\n") {
		t.Fatalf("stdout = %q, want the summary line", stdout)
	}
	if !strings.Contains(stderr, "refusing to delete") || !strings.Contains(stderr, outsidePath) {
		t.Fatalf("stderr = %q, want the refusal naming the outside path", stderr)
	}
	if _, err := os.Stat(outsidePath); err != nil {
		t.Fatalf("the outside file was deleted: %v", err)
	}
	if _, err := os.Stat(realPath); !os.IsNotExist(err) {
		t.Fatalf("in-directory record %s still exists: %v", realPath, err)
	}
}

// TestRunsPruneDeleteFailureNonJSONLContinuesAndExits9 asserts the
// second refusal arm of the delete path the same way the outside-path
// test asserts the first: a listed record whose path is a real
// non-.jsonl file inside the records directory is refused — the
// .jsonl rule re-validates every path immediately before os.Remove —
// stays on disk byte-identical, and is named on stderr, while the
// listed record that is a real .jsonl record is still deleted and the
// exit is 9. The refusal is injected through the runlogList seam,
// exactly as the seam exists to be.
func TestRunsPruneDeleteFailureNonJSONLContinuesAndExits9(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	realPath := writeListRecord(t, dir, "20260830T103000Z-1", deadPID, "2026-08-30T10:30:00Z", "/ws-a", 1, true, "completed", "")
	notesPath := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notesPath, []byte("not a record\n"), 0o600); err != nil {
		t.Fatalf("write notes: %v", err)
	}
	original := runlogList
	runlogList = func(string) ([]runlog.Run, error) {
		return []runlog.Run{
			{RunID: "20260830T103000Z-1", Outcome: "completed", Path: realPath},
			{RunID: "20260830T101500Z-0", Outcome: "completed", Path: notesPath},
		}, nil
	}
	t.Cleanup(func() { runlogList = original })

	code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0", "--yes"}, "")
	if code != 9 {
		t.Fatalf("exit = %d, want 9; stdout = %q; stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "deleted 20260830T103000Z-1\n") {
		t.Fatalf("stdout = %q, want the deleted line for the record that went", stdout)
	}
	if !strings.Contains(stdout, "kept 0 newest\n") {
		t.Fatalf("stdout = %q, want the summary line", stdout)
	}
	if !strings.Contains(stderr, "refusing to delete") || !strings.Contains(stderr, notesPath) {
		t.Fatalf("stderr = %q, want the refusal naming the non-.jsonl path", stderr)
	}
	data, err := os.ReadFile(notesPath)
	if err != nil {
		t.Fatalf("the non-record file was deleted: %v", err)
	}
	if string(data) != "not a record\n" {
		t.Fatalf("the non-record file changed by the prune: %q", data)
	}
	if _, err := os.Stat(realPath); !os.IsNotExist(err) {
		t.Fatalf("in-directory record %s still exists: %v", realPath, err)
	}
}

// TestRunsPruneNothingToPrune asserts the empty cases are successes,
// not errors, and need neither --yes nor a terminal: nothing was
// selected, so there is nothing to ask about. That includes a records
// directory that does not exist and a directory whose only records
// belong to still-running runs.
func TestRunsPruneNothingToPrune(t *testing.T) {
	t.Run("empty records directory", func(t *testing.T) {
		dir := t.TempDir()
		withRunlogDir(t, dir)
		for _, args := range [][]string{
			{"runs", "prune", "--keep", "0", "--yes"},
			{"runs", "prune", "--keep", "0"},
		} {
			code, stdout, stderr := runCLI(t, args, "")
			if code != 0 || stdout != "nothing to prune\n" || stderr != "" {
				t.Fatalf("%v = (%d, %q, %q), want (0, \"nothing to prune\\n\", \"\")", args, code, stdout, stderr)
			}
		}
	})

	t.Run("missing records directory", func(t *testing.T) {
		withRunlogDir(t, filepath.Join(t.TempDir(), "missing"))
		code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "3", "--yes"}, "")
		if code != 0 || stdout != "nothing to prune\n" || stderr != "" {
			t.Fatalf("runs prune = (%d, %q, %q)", code, stdout, stderr)
		}
	})

	t.Run("only running records", func(t *testing.T) {
		dir := t.TempDir()
		withRunlogDir(t, dir)
		path := writeListRecord(t, dir, "20260830T103000Z-3", os.Getpid(), "2026-08-30T10:30:00Z", "/ws-a", 1, false, "", "")
		for _, args := range [][]string{
			{"runs", "prune", "--keep", "0", "--yes"},
			{"runs", "prune", "--keep", "0"},
		} {
			code, stdout, stderr := runCLI(t, args, "")
			if code != 0 || stdout != "nothing to prune\n" || stderr != "" {
				t.Fatalf("%v = (%d, %q, %q), want (0, \"nothing to prune\\n\", \"\")", args, code, stdout, stderr)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("running record %s vanished: %v", path, err)
			}
		}
	})
}

// TestRunsPruneUsageErrors asserts every argv mistake exits 2 with the
// usage on stderr and nothing on stdout: a missing --keep, a
// non-integer, a negative number in both spellings, a repeat, a
// value given to the value-less --yes, an unknown flag, an extra
// argument, a missing value, and the prune-only flags on runs list.
func TestRunsPruneUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"runs", "prune"},
		{"runs", "prune", "--keep", "abc"},
		{"runs", "prune", "--keep", "-1"},
		{"runs", "prune", "--keep=-1"},
		{"runs", "prune", "--keep", "1", "--keep", "1"},
		{"runs", "prune", "--keep", "1", "--yes=1"},
		{"runs", "prune", "--keep", "1", "--bogus"},
		{"runs", "prune", "--keep", "1", "extra"},
		{"runs", "prune", "--keep"},
		{"runs", "list", "--keep", "1"},
		{"runs", "list", "--yes"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, stdout, stderr := runCLI(t, args, "")
			if code != 2 || stdout != "" || !strings.Contains(stderr, "pi-worker:") {
				t.Fatalf("%v = (%d, %q, %q), want exit 2 with stderr", args, code, stdout, stderr)
			}
		})
	}
}

// TestRunsPruneKeepEqualsValueSpelling asserts --keep=3 behaves
// exactly like --keep 3: the same records are deleted in the same
// order and the same summary is printed.
func TestRunsPruneKeepEqualsValueSpelling(t *testing.T) {
	for _, args := range [][]string{
		{"runs", "prune", "--keep", "2", "--yes"},
		{"runs", "prune", "--keep=2", "--yes"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			dir := t.TempDir()
			withRunlogDir(t, dir)
			writeListRecord(t, dir, "20260830T104000Z-4", deadPID, "2026-08-30T10:40:00Z", "/ws-d", 1, true, "completed", "")
			writeListRecord(t, dir, "20260830T103000Z-3", deadPID, "2026-08-30T10:30:00Z", "/ws-c", 1, true, "completed", "")
			writeListRecord(t, dir, "20260830T102000Z-2", deadPID, "2026-08-30T10:20:00Z", "/ws-b", 1, true, "completed", "")
			writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/ws-a", 1, true, "completed", "")

			code, stdout, stderr := runCLI(t, args, "")
			if code != 0 || stderr != "" {
				t.Fatalf("%v = (%d, %q, %q)", args, code, stdout, stderr)
			}
			want := "deleted 20260830T101500Z-1\ndeleted 20260830T102000Z-2\nkept 2 newest\n"
			if stdout != want {
				t.Fatalf("%v: stdout = %q, want %q", args, stdout, want)
			}
		})
	}
}

// TestRunsPruneWiredIntoMainWithContext asserts the second dispatch
// switch — mainWithContext, the seam the cancellation tests drive —
// routes runs prune the same way Main does, stdin included.
func TestRunsPruneWiredIntoMainWithContext(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	oldPath := writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/ws-a", 1, true, "completed", "")
	newestPath := writeListRecord(t, dir, "20260830T103000Z-3", deadPID, "2026-08-30T10:30:00Z", "/ws-b", 1, true, "completed", "")

	code, stdout, stderr := runCLIWithContext(t, context.Background(), []string{"runs", "prune", "--keep", "1", "--yes"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("mainWithContext runs prune = (%d, %q, %q)", code, stdout, stderr)
	}
	if want := "deleted 20260830T101500Z-1\nkept 1 newest\n"; stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("record %s still exists: %v", oldPath, err)
	}
	if _, err := os.Stat(newestPath); err != nil {
		t.Fatalf("record %s vanished: %v", newestPath, err)
	}
}

// TestRunsPruneResolverAndReadFailuresExit9 asserts both failure
// classes exit 9 before anything is deleted: a records directory that
// cannot be resolved and one that exists but cannot be read.
func TestRunsPruneResolverAndReadFailuresExit9(t *testing.T) {
	t.Run("resolver failure", func(t *testing.T) {
		original := runlogDir
		runlogDir = func() (string, error) { return "", fmt.Errorf("config unavailable") }
		t.Cleanup(func() { runlogDir = original })

		code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0", "--yes"}, "")
		if code != 9 || stdout != "" || !strings.Contains(stderr, "determine records directory") {
			t.Fatalf("resolver failure = (%d, %q, %q), want exit 9", code, stdout, stderr)
		}
	})

	t.Run("unreadable records directory", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("permission bits are not enforced on Windows")
		}
		dir := t.TempDir()
		writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/ws-a", 1, true, "completed", "")
		if err := os.Chmod(dir, 0o000); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o700) })
		withRunlogDir(t, dir)

		code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0", "--yes"}, "")
		if code != 9 || stdout != "" || !strings.Contains(stderr, "list runs") {
			t.Fatalf("unreadable dir = (%d, %q, %q), want exit 9", code, stdout, stderr)
		}
	})
}

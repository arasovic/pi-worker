package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

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
// record is still spared. The unknown record's file is deliberately
// aged past the grace window, because a fresh one is spared and has
// its own test.
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
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(unknownPath, stale, stale); err != nil {
		t.Fatalf("chtimes: %v", err)
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

// TestRunsPruneSparesFreshUnknownCandidate asserts the live-window
// rule for records that cannot be classified: an unknown candidate
// whose file was modified within the grace window may be a run that
// just created its record and has not yet written its start line, so
// it is kept — not deleted. With nothing selected the human summary
// says nothing to prune; the spared record is visible to a caller in
// the --json document, whose keptUnreadable array carries its id —
// never the keptRunning array, which carries only the still-running
// runs' ids: the one thing known about this record is that it could
// not be read, and no report may call it running.
func TestRunsPruneSparesFreshUnknownCandidate(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	unknownPath := filepath.Join(dir, "20260831T120000Z-1.jsonl")
	if err := os.WriteFile(unknownPath, []byte(""), 0o600); err != nil {
		t.Fatalf("write empty record: %v", err)
	}

	code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0", "--yes"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("runs prune = (%d, %q, %q), want exit 0 with empty stderr", code, stdout, stderr)
	}
	if stdout != "nothing to prune\n" {
		t.Fatalf("stdout = %q, want \"nothing to prune\\n\"", stdout)
	}
	if _, err := os.Stat(unknownPath); err != nil {
		t.Fatalf("fresh unknown record %s was deleted: %v", unknownPath, err)
	}

	// The second call runs prune --json: the spared record is visible
	// to a caller only in the document, and the document is decoded,
	// never compared as a raw string.
	code, stdout, stderr = runCLI(t, []string{"runs", "prune", "--keep", "0", "--yes", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("runs prune --json = (%d, %q, %q), want exit 0 with empty stderr", code, stdout, stderr)
	}
	var document struct {
		SchemaVersion  int      `json:"schemaVersion"`
		Deleted        []string `json:"deleted"`
		KeptNewest     int      `json:"keptNewest"`
		KeptRunning    []string `json:"keptRunning"`
		KeptUnreadable []string `json:"keptUnreadable"`
	}
	if err := json.Unmarshal([]byte(stdout), &document); err != nil {
		t.Fatalf("decode document %q: %v", stdout, err)
	}
	if document.SchemaVersion != 1 || len(document.Deleted) != 0 || document.KeptNewest != 0 || len(document.KeptRunning) != 0 || len(document.KeptUnreadable) != 1 || document.KeptUnreadable[0] != "20260831T120000Z-1" {
		t.Fatalf("document = %#v, want the spared record in keptUnreadable and an empty keptRunning", document)
	}
	if _, err := os.Stat(unknownPath); err != nil {
		t.Fatalf("fresh unknown record %s was deleted by the --json prune: %v", unknownPath, err)
	}
}

// TestRunsPruneDeletesStaleUnknownCandidate asserts the live-window
// rule's far side: the same empty record, its modification time pushed
// two hours into the past, is unreadable junk and is deleted — the
// junk-clearing behaviour survives the grace window.
func TestRunsPruneDeletesStaleUnknownCandidate(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	unknownPath := filepath.Join(dir, "20260831T120000Z-1.jsonl")
	if err := os.WriteFile(unknownPath, []byte(""), 0o600); err != nil {
		t.Fatalf("write empty record: %v", err)
	}
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(unknownPath, stale, stale); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0", "--yes"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("runs prune = (%d, %q, %q), want exit 0 with empty stderr", code, stdout, stderr)
	}
	const want = "deleted 20260831T120000Z-1\nkept 0 newest\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	if _, err := os.Stat(unknownPath); !os.IsNotExist(err) {
		t.Fatalf("stale unknown record %s still exists: %v", unknownPath, err)
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

// TestRunsPruneJSONWithoutYesRefusesWhenNothingSelected asserts the
// --json refusal does not depend on what is on disk: with --json and
// no --yes in an empty records directory, prune still exits 2 with the
// verbatim refusal on stderr and no JSON document on stdout — a script
// must be able to tell "refused" from "succeeded, nothing to do".
func TestRunsPruneJSONWithoutYesRefusesWhenNothingSelected(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)

	code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0", "--json"}, "")
	if code != 2 || stdout != "" {
		t.Fatalf("runs prune --json = (%d, %q), want exit 2 with nothing on stdout", code, stdout)
	}
	if want := "pi-worker: runs prune needs --yes when it cannot ask\n"; stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
}

// TestRunsPruneJSONWithoutYesRefusesBeforeDirectoryResolved asserts
// the --json refusal does not depend on the records directory at all:
// with --json and no --yes, prune refuses with exit 2 before the
// directory is even resolved, so a resolver failure or an unreadable
// directory cannot turn the refusal into an exit 9. The refusal
// depends on nothing but the flags.
func TestRunsPruneJSONWithoutYesRefusesBeforeDirectoryResolved(t *testing.T) {
	t.Run("resolver failure", func(t *testing.T) {
		original := runlogDir
		runlogDir = func() (string, error) { return "", fmt.Errorf("config unavailable") }
		t.Cleanup(func() { runlogDir = original })

		code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0", "--json"}, "")
		if code != 2 || stdout != "" {
			t.Fatalf("resolver failure with --json and no --yes = (%d, %q, %q), want exit 2 with nothing on stdout", code, stdout, stderr)
		}
		if want := "pi-worker: runs prune needs --yes when it cannot ask\n"; stderr != want {
			t.Fatalf("stderr = %q, want %q", stderr, want)
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

		code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0", "--json"}, "")
		if code != 2 || stdout != "" {
			t.Fatalf("unreadable dir with --json and no --yes = (%d, %q, %q), want exit 2 with nothing on stdout", code, stdout, stderr)
		}
		if want := "pi-worker: runs prune needs --yes when it cannot ask\n"; stderr != want {
			t.Fatalf("stderr = %q, want %q", stderr, want)
		}
	})
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
// exact field names and shape, deleted, keptRunning, and
// keptUnreadable as arrays of run ids — empty arrays, never null,
// whether nothing was deleted, no running run was spared, or nothing
// unreadable was spared — and keptNewest as how many the --keep rule
// kept, capped by the records that exist.
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
		want := "{\"schemaVersion\":1,\"deleted\":[\"20260830T101500Z-1\"],\"keptNewest\":1,\"keptRunning\":[\"20260830T102000Z-2\"],\"keptUnreadable\":[]}\n"
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
		want := "{\"schemaVersion\":1,\"deleted\":[],\"keptNewest\":3,\"keptRunning\":[],\"keptUnreadable\":[]}\n"
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
		want := "{\"schemaVersion\":1,\"deleted\":[],\"keptNewest\":0,\"keptRunning\":[],\"keptUnreadable\":[]}\n"
		if stdout != want {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
	})
}

// TestRunsPruneKeptRunningAndKeptUnreadableApart asserts the two
// spared classes report apart in one prune: a candidate whose run is
// still running and a candidate too unreadable to classify whose file
// changed within the grace window are both spared, and each id lands
// in its own bucket — the running one in keptRunning, the unreadable
// one in keptUnreadable, never mixed — in the --json document, and
// the human summary carries one clause per class. A third, deletable
// record makes the prune delete something, so the summary line is
// rendered at all.
func TestRunsPruneKeptRunningAndKeptUnreadableApart(t *testing.T) {
	// One fresh records directory per subtest: a prune that deleted
	// something in an earlier subtest must not change what a later
	// subtest counts.
	withRecords := func(t *testing.T) (dir string) {
		t.Helper()
		dir = t.TempDir()
		withRunlogDir(t, dir)
		writeListRecord(t, dir, "20260830T103000Z-3", os.Getpid(), "2026-08-30T10:30:00Z", "/ws-c", 1, false, "", "")
		unknownPath := filepath.Join(dir, "20260830T102000Z-2.jsonl")
		if err := os.WriteFile(unknownPath, []byte("not json at all\n"), 0o600); err != nil {
			t.Fatalf("write unknown record: %v", err)
		}
		writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/ws-a", 1, true, "completed", "")
		return dir
	}

	t.Run("--json document", func(t *testing.T) {
		dir := withRecords(t)
		code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0", "--yes", "--json"}, "")
		if code != 0 || stderr != "" {
			t.Fatalf("runs prune --json = (%d, %q, %q)", code, stdout, stderr)
		}
		want := "{\"schemaVersion\":1,\"deleted\":[\"20260830T101500Z-1\"],\"keptNewest\":0,\"keptRunning\":[\"20260830T103000Z-3\"],\"keptUnreadable\":[\"20260830T102000Z-2\"]}\n"
		if stdout != want {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
		if _, err := os.Stat(filepath.Join(dir, "20260830T101500Z-1.jsonl")); !os.IsNotExist(err) {
			t.Fatalf("deleted record still exists after the prune: %v", err)
		}
		for _, kept := range []string{"20260830T103000Z-3.jsonl", "20260830T102000Z-2.jsonl"} {
			if _, err := os.Stat(filepath.Join(dir, kept)); err != nil {
				t.Fatalf("kept record %s vanished: %v", kept, err)
			}
		}
	})

	t.Run("human summary", func(t *testing.T) {
		dir := withRecords(t)
		code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0", "--yes"}, "")
		if code != 0 || stderr != "" {
			t.Fatalf("runs prune = (%d, %q, %q), want exit 0 with empty stderr", code, stdout, stderr)
		}
		const want = "deleted 20260830T101500Z-1\nkept 0 newest, 1 still running, 1 unreadable\n"
		if stdout != want {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
		if _, err := os.Stat(filepath.Join(dir, "20260830T101500Z-1.jsonl")); !os.IsNotExist(err) {
			t.Fatalf("deleted record still exists after the prune: %v", err)
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

// TestRunsPruneCancelledBeforeStartDeletesNothingAndExits9 asserts a
// prune whose context is already done deletes nothing: the context is
// read immediately after the prompt returns and again before every
// individual delete, so a cancellation that arrived before the command
// started means no record is removed, the verbatim message goes to
// stderr, and the exit is 9. Three paths are pinned: --yes (the check
// sitting in front of the first delete), the y answer to the prompt
// (the check on the way out of it), and --json (the document still
// reports what was deleted — an empty deleted array — on the way to
// exit 9).
func TestRunsPruneCancelledBeforeStartDeletesNothingAndExits9(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("--yes", func(t *testing.T) {
		dir := t.TempDir()
		withRunlogDir(t, dir)
		path := writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/ws-a", 1, true, "completed", "")

		code, stdout, stderr := runCLIWithContext(t, ctx, []string{"runs", "prune", "--keep", "0", "--yes"}, "")
		if code != 9 || stdout != "" {
			t.Fatalf("cancelled prune --yes = (%d, %q, %q), want exit 9 with nothing on stdout", code, stdout, stderr)
		}
		if want := "pi-worker: runs prune cancelled\n"; stderr != want {
			t.Fatalf("stderr = %q, want %q", stderr, want)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("record %s vanished after a cancelled prune: %v", path, err)
		}
	})

	t.Run("prompt answered y", func(t *testing.T) {
		dir := t.TempDir()
		withRunlogDir(t, dir)
		path := writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/ws-a", 1, true, "completed", "")
		withStdinIsTerminal(t, true)

		code, stdout, stderr := runCLIWithContext(t, ctx, []string{"runs", "prune", "--keep", "0"}, "y\n")
		if code != 9 || stdout != "" {
			t.Fatalf("cancelled prune after y = (%d, %q, %q), want exit 9 with nothing on stdout", code, stdout, stderr)
		}
		if want := "pi-worker: runs prune cancelled\n"; !strings.HasSuffix(stderr, want) {
			t.Fatalf("stderr = %q, want it to end with %q", stderr, want)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("record %s vanished after a cancelled prune: %v", path, err)
		}
	})

	t.Run("--json", func(t *testing.T) {
		dir := t.TempDir()
		withRunlogDir(t, dir)
		path := writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/ws-a", 1, true, "completed", "")

		code, stdout, stderr := runCLIWithContext(t, ctx, []string{"runs", "prune", "--keep", "0", "--yes", "--json"}, "")
		if code != 9 {
			t.Fatalf("cancelled prune --json = (%d, %q, %q), want exit 9", code, stdout, stderr)
		}
		if want := "{\"schemaVersion\":1,\"deleted\":[],\"keptNewest\":0,\"keptRunning\":[],\"keptUnreadable\":[]}\n"; stdout != want {
			t.Fatalf("stdout = %q, want the empty document %q", stdout, want)
		}
		if want := "pi-worker: runs prune cancelled\n"; stderr != want {
			t.Fatalf("stderr = %q, want %q", stderr, want)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("record %s vanished after a cancelled prune: %v", path, err)
		}
	})
}

// promptRecorder records stderr writes and closes a channel the
// moment the question line has been written in full, so a test can
// cancel a blocked prompt exactly when it is on screen — never after
// a sleep. The recorded bytes are read back only after the command
// has returned, when the channel the command's exit travels on has
// already ordered the writes.
type promptRecorder struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	question string
	seen     chan struct{}
	reported bool
}

func (p *promptRecorder) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf.Write(b)
	if !p.reported && strings.Contains(p.buf.String(), p.question) {
		p.reported = true
		close(p.seen)
	}
	return len(b), nil
}

func (p *promptRecorder) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buf.String()
}

// TestRunsPruneCancelledWhilePromptBlockedExits9AndDeletesNothing
// pins the prompt's interruptibility: stdin is a pipe whose write end
// never receives a byte, the command blocks on the question, and the
// test cancels the context the moment the question has been written.
// The cancelled context wins over the never-arriving answer: exit 9,
// the verbatim cancelled message on stderr, nothing on stdout, and
// the record still on disk. The reader goroutine stays parked on the
// pipe for the rest of the test process's life — the accepted ceiling
// — and the pipe's write end is closed in cleanup so the goroutine
// can leave once the test is done with it.
func TestRunsPruneCancelledWhilePromptBlockedExits9AndDeletesNothing(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	path := writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/ws-a", 1, true, "completed", "")
	withStdinIsTerminal(t, true)

	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	recorder := &promptRecorder{question: "delete 1 run records? [y/N] ", seen: make(chan struct{})}
	var stdout bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- mainWithContext(ctx, []string{"runs", "prune", "--keep", "0"}, pr, &stdout, recorder)
	}()

	select {
	case <-recorder.seen:
	case <-time.After(10 * time.Second):
		t.Fatal("the deletion question never appeared on stderr")
	}
	cancel()
	var code int
	select {
	case code = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("prune did not stop after the cancellation while the prompt was blocked")
	}
	if code != 9 {
		t.Fatalf("exit = %d, want 9; stdout = %q; stderr = %q", code, stdout.String(), recorder.String())
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want nothing", stdout.String())
	}
	const wantStderr = "RUN ID                STARTED                 OUTCOME      TASKS    WORKSPACE\n" +
		"20260830T101500Z-1    2026-08-30T10:15:00Z    completed    1        /ws-a\n" +
		"delete 1 run records? [y/N] " +
		"pi-worker: runs prune cancelled\n"
	if recorder.String() != wantStderr {
		t.Fatalf("stderr = %q, want %q", recorder.String(), wantStderr)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("record %s vanished after a cancelled prune: %v", path, err)
	}
}

// TestRunsPruneCancelledWithEmptySelectionExits9 asserts a prune
// whose context is cancelled and whose selection is empty exits 9,
// never 0: the listing may take a while, the person cancels during
// it, and the empty selection must not turn the cancelled command
// into a finished one. Both report arms are pinned — the human
// "nothing to prune" line must not appear, and the --json arm still
// renders the empty document, the way the cancelled path always
// reports what was deleted.
func TestRunsPruneCancelledWithEmptySelectionExits9(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("human", func(t *testing.T) {
		dir := t.TempDir()
		withRunlogDir(t, dir)

		code, stdout, stderr := runCLIWithContext(t, ctx, []string{"runs", "prune", "--keep", "0", "--yes"}, "")
		if code != 9 || stdout != "" {
			t.Fatalf("cancelled prune with empty selection = (%d, %q, %q), want exit 9 with nothing on stdout", code, stdout, stderr)
		}
		if want := "pi-worker: runs prune cancelled\n"; stderr != want {
			t.Fatalf("stderr = %q, want %q", stderr, want)
		}
	})

	t.Run("--json", func(t *testing.T) {
		dir := t.TempDir()
		withRunlogDir(t, dir)

		code, stdout, stderr := runCLIWithContext(t, ctx, []string{"runs", "prune", "--keep", "0", "--yes", "--json"}, "")
		if code != 9 {
			t.Fatalf("cancelled prune --json with empty selection = (%d, %q, %q), want exit 9", code, stdout, stderr)
		}
		if want := "{\"schemaVersion\":1,\"deleted\":[],\"keptNewest\":0,\"keptRunning\":[],\"keptUnreadable\":[]}\n"; stdout != want {
			t.Fatalf("stdout = %q, want the empty document %q", stdout, want)
		}
		if want := "pi-worker: runs prune cancelled\n"; stderr != want {
			t.Fatalf("stderr = %q, want %q", stderr, want)
		}
	})
}

// cancelAfterDeletedLine cancels the context the moment the deleted
// line for targetID has been written in full, so a test can land a
// cancellation exactly after the final delete — deterministic, no
// sleeps. The buffer is written only by the goroutine running the
// command, which is the same goroutine calling cancel, so no locking
// is needed.
type cancelAfterDeletedLine struct {
	buf      bytes.Buffer
	targetID string
	cancel   context.CancelFunc
}

func (c *cancelAfterDeletedLine) Write(b []byte) (int, error) {
	c.buf.Write(b)
	if c.targetID != "" && strings.Contains(c.buf.String(), "deleted "+c.targetID+"\n") {
		c.targetID = ""
		c.cancel()
	}
	return len(b), nil
}

// TestRunsPruneCancelledAfterFinalDeleteExits9AndReportsDeleted
// asserts a cancellation that lands after the last delete still exits
// 9 and never claims the prune finished: the loop's context check
// sits before each delete, so the command needs one more check after
// the loop. The deleted lines are already on stdout — each record
// reported as it went — and cancelledPrune adds the verbatim message
// on stderr; the summary line must not appear.
func TestRunsPruneCancelledAfterFinalDeleteExits9AndReportsDeleted(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	oldPath := writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/ws-a", 1, true, "completed", "")
	newestPath := writeListRecord(t, dir, "20260830T103000Z-3", deadPID, "2026-08-30T10:30:00Z", "/ws-c", 1, true, "completed", "")

	ctx, cancel := context.WithCancel(context.Background())
	var stderr bytes.Buffer
	// --keep 0 selects both records; deleting oldest first, the newest
	// record's deleted line is the last one written, so the cancel
	// lands after the final delete.
	stdout := &cancelAfterDeletedLine{targetID: "20260830T103000Z-3", cancel: cancel}
	code := mainWithContext(ctx, []string{"runs", "prune", "--keep", "0", "--yes"}, strings.NewReader(""), stdout, &stderr)
	if code != 9 {
		t.Fatalf("prune cancelled after the final delete = (%d, %q, %q), want exit 9", code, stdout.buf.String(), stderr.String())
	}
	if want := "deleted 20260830T101500Z-1\ndeleted 20260830T103000Z-3\n"; stdout.buf.String() != want {
		t.Fatalf("stdout = %q, want the deleted lines %q and no summary", stdout.buf.String(), want)
	}
	if want := "pi-worker: runs prune cancelled\n"; stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
	for _, gone := range []string{oldPath, newestPath} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("record %s still exists: %v", gone, err)
		}
	}
}

// cancelledButDoneSilentContext is the one context combination that
// reaches the prompt's answer path with a cancelled context: Err
// reports the cancellation while Done never closes, so the select
// cannot see the cancellation and the answer is taken. The check
// after the answer reads Err, which is the only thing that can catch
// a cancellation this context reports — pinning that check
// deterministically, where a real cancelled context would leave the
// select to flip a coin between two ready cases.
type cancelledButDoneSilentContext struct{}

func (cancelledButDoneSilentContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (cancelledButDoneSilentContext) Done() <-chan struct{}       { return nil }
func (cancelledButDoneSilentContext) Err() error                  { return context.Canceled }
func (cancelledButDoneSilentContext) Value(any) any               { return nil }

// TestRunsPruneDecliningAnswerAfterCancellationExits9 asserts the
// prompt's answer path re-reads the context before reporting success:
// a declining answer taken while the context reports cancelled must
// end in the cancelled path — nothing deleted, the verbatim message
// on stderr, exit 9 — never in a finished "nothing deleted".
func TestRunsPruneDecliningAnswerAfterCancellationExits9(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	path := writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/ws-a", 1, true, "completed", "")
	withStdinIsTerminal(t, true)

	code, stdout, stderr := runCLIWithContext(t, cancelledButDoneSilentContext{}, []string{"runs", "prune", "--keep", "0"}, "n\n")
	if code != 9 || stdout != "" {
		t.Fatalf("declining answer with a cancelled context = (%d, %q, %q), want exit 9 with nothing on stdout", code, stdout, stderr)
	}
	if want := "pi-worker: runs prune cancelled\n"; !strings.HasSuffix(stderr, want) {
		t.Fatalf("stderr = %q, want it to end with %q", stderr, want)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("record %s vanished: %v", path, err)
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

// swapRecordsDirOnFirstRead swaps the records directory out from under
// the command on the first stdin Read: dir is renamed to aside and a
// symlink to other is put in its place, then the answer "y" is
// returned. The prompt's ReadString is the first read of stdin, so the
// swap lands exactly while the question is on screen — deterministic,
// no sleeps, no timing. The one Read that matters carries the answer
// and the newline, so bufio never reads again; a failed rename or
// symlink is returned as the Read error and checked by the test after
// the command returns.
//
// Because the first Read happens inside the prompt, the swap also
// proves the order the fix pins: a records directory resolved before
// the question cannot be redirected afterwards, and a delete that
// resolved the directory again after the swap would land in other.
// All three paths are t.TempDir-managed, and cleanup removes the
// symlink as a link, never through it.
//
// The records directory here is the one runlogDir reports; the swap
// renames it to aside and symlinks other into its place.
type swapRecordsDirOnFirstRead struct {
	dir, aside, other string
	swapped           bool
	err               error
}

func (s *swapRecordsDirOnFirstRead) Read(p []byte) (int, error) {
	if !s.swapped {
		s.swapped = true
		if err := os.Rename(s.dir, s.aside); err != nil {
			s.err = err
			return 0, err
		}
		if err := os.Symlink(s.other, s.dir); err != nil {
			s.err = err
			return 0, err
		}
	}
	return copy(p, "y\n"), io.EOF
}

// TestRunsPruneSwapDuringPromptCannotRedirectTheDelete swaps the
// records directory out from under the prune while the deletion
// question is on screen — the directory is renamed aside, a symlink
// to an unrelated directory is put in its place, and then "y" is
// answered — and pins that the delete still lands only inside the
// directory the listing named: the unrelated directory's file with
// the same base name survives, the record the command named is gone
// from the renamed-aside directory, and the exit is 0. A prune that
// resolved the directory again after the prompt would delete the
// unrelated directory's file instead.
func TestRunsPruneSwapDuringPromptCannotRedirectTheDelete(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	withStdinIsTerminal(t, true)
	recordName := "20260830T101500Z-1.jsonl"
	writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/ws-a", 1, true, "completed", "")

	// The unrelated directory holds a file with the same base name as
	// the record being deleted; a delete redirected by the swap would
	// land on exactly this file.
	other := t.TempDir()
	otherPath := filepath.Join(other, recordName)
	if err := os.WriteFile(otherPath, []byte("not a record"), 0o600); err != nil {
		t.Fatalf("write decoy: %v", err)
	}

	// aside receives the records directory when the swap renames it
	// away from dir; the empty placeholder is removed so the rename
	// replaces it, and the whole swap stays inside t.TempDir-managed
	// paths — cleanup removes the symlink as a link and the renamed
	// directory with its contents.
	aside := t.TempDir()
	if err := os.Remove(aside); err != nil {
		t.Fatalf("vacate aside: %v", err)
	}

	stdin := &swapRecordsDirOnFirstRead{dir: dir, aside: aside, other: other}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := mainWithContext(context.Background(), []string{"runs", "prune", "--keep", "0"}, stdin, &stdout, &stderr)
	if stdin.err != nil {
		t.Fatalf("swap during the prompt failed: %v", stdin.err)
	}
	if code != 0 {
		t.Fatalf("prune with the swap during the prompt = (%d, %q, %q), want exit 0", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(otherPath); err != nil {
		t.Fatalf("%s was deleted although prune never listed %s: %v", otherPath, dir, err)
	}
	if _, err := os.Stat(filepath.Join(aside, recordName)); err == nil {
		t.Fatalf("record %s still exists in the listed directory after the prune", filepath.Join(aside, recordName))
	}
}

// replaceRecordWithDirectoryOnFirstRead removes the record file and
// puts an empty directory of the same name in its place on the first
// stdin Read, then answers "y". The prompt's ReadString is the first
// read of stdin, so the replacement lands exactly while the question
// is on screen — deterministic, no sleeps — the same window
// swapRecordsDirOnFirstRead uses. The replacement directory is left
// empty, which is what a renamed-away record's name can be replaced
// with.
type replaceRecordWithDirectoryOnFirstRead struct {
	recordPath string
	err        error
}

func (r *replaceRecordWithDirectoryOnFirstRead) Read(p []byte) (int, error) {
	if err := os.Remove(r.recordPath); err != nil {
		r.err = err
		return 0, err
	}
	if err := os.Mkdir(r.recordPath, 0o700); err != nil {
		r.err = err
		return 0, err
	}
	return copy(p, "y\n"), io.EOF
}

// TestRunsPruneRecordReplacedByDirectoryDuringPromptRefused asserts
// the delete-time gate's identity check: the listed record's name is
// replaced by an empty directory while the question is on screen,
// the answer is y, and prune refuses — exit 9, the refusal naming
// that the name no longer holds the file that was listed, the
// directory still there, and no deleted line for the run id. A
// removal without the gate would rmdir the empty replacement and
// print "deleted <run id>" for a record that was long gone.
func TestRunsPruneRecordReplacedByDirectoryDuringPromptRefused(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	withStdinIsTerminal(t, true)
	const runID = "20260830T101500Z-1"
	recordPath := writeListRecord(t, dir, runID, deadPID, "2026-08-30T10:15:00Z", "/ws-a", 1, true, "completed", "")

	stdin := &replaceRecordWithDirectoryOnFirstRead{recordPath: recordPath}
	var stdout, stderr bytes.Buffer
	code := mainWithContext(context.Background(), []string{"runs", "prune", "--keep", "0"}, stdin, &stdout, &stderr)
	if stdin.err != nil {
		t.Fatalf("replacement during the prompt failed: %v", stdin.err)
	}
	if code != 9 {
		t.Fatalf("prune with the record replaced by a directory = (%d, %q, %q), want exit 9", code, stdout.String(), stderr.String())
	}
	if info, err := os.Lstat(recordPath); err != nil {
		t.Fatalf("the replacement directory %s vanished: %v", recordPath, err)
	} else if !info.IsDir() {
		t.Fatalf("%s is not a directory after the prune", recordPath)
	}
	if want := "kept 0 newest\n"; stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
	if !strings.Contains(stderr.String(), "no longer the record that was listed") {
		t.Fatalf("stderr = %q, want the identity refusal naming the replacement", stderr.String())
	}
}

// replaceRecordWithSymlinkOnFirstRead removes the record file and
// puts a symlink of the same name pointing at targetPath in its place
// on the first stdin Read, then answers "y", exactly like
// replaceRecordWithDirectoryOnFirstRead in the same window.
type replaceRecordWithSymlinkOnFirstRead struct {
	recordPath string
	targetPath string
	err        error
}

func (r *replaceRecordWithSymlinkOnFirstRead) Read(p []byte) (int, error) {
	if err := os.Remove(r.recordPath); err != nil {
		r.err = err
		return 0, err
	}
	if err := os.Symlink(r.targetPath, r.recordPath); err != nil {
		r.err = err
		return 0, err
	}
	return copy(p, "y\n"), io.EOF
}

// TestRunsPruneRecordReplacedBySymlinkDuringPromptRefused asserts
// the identity gate for a symlink replacement: the listed record's
// name is replaced by a symlink pointing outside the records
// directory while the question is on screen, the answer is y, and
// prune refuses — exit 9, the refusal naming that the name no longer
// holds the file that was listed, and both the link and its target
// survive, byte-identical. The refusal is identity, not type: a
// symlink that was what the listing showed is deleted, and one that
// merely appears under the listed name is not.
func TestRunsPruneRecordReplacedBySymlinkDuringPromptRefused(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	withStdinIsTerminal(t, true)
	const runID = "20260830T101500Z-1"
	recordPath := writeListRecord(t, dir, runID, deadPID, "2026-08-30T10:15:00Z", "/ws-a", 1, true, "completed", "")

	targetDir := t.TempDir()
	targetPath := filepath.Join(targetDir, "precious.jsonl")
	if err := os.WriteFile(targetPath, []byte("not a record\n"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}

	stdin := &replaceRecordWithSymlinkOnFirstRead{recordPath: recordPath, targetPath: targetPath}
	var stdout, stderr bytes.Buffer
	code := mainWithContext(context.Background(), []string{"runs", "prune", "--keep", "0"}, stdin, &stdout, &stderr)
	if stdin.err != nil {
		t.Fatalf("replacement during the prompt failed: %v", stdin.err)
	}
	if code != 9 {
		t.Fatalf("prune with the record replaced by a symlink = (%d, %q, %q), want exit 9", code, stdout.String(), stderr.String())
	}
	if info, err := os.Lstat(recordPath); err != nil {
		t.Fatalf("the replacement symlink %s is gone: %v", recordPath, err)
	} else if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink after the prune", recordPath)
	}
	data, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("the symlink's target %s was touched: %v", targetPath, err)
	}
	if string(data) != "not a record\n" {
		t.Fatalf("the target changed by the prune: %q", data)
	}
	if want := "kept 0 newest\n"; stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
	if !strings.Contains(stderr.String(), "no longer the record that was listed") {
		t.Fatalf("stderr = %q, want the identity refusal naming the replacement", stderr.String())
	}
}

// removeRecordOnFirstRead deletes the record file on the first stdin
// Read, then answers "y": the record is gone exactly while the
// question is on screen, so the gate's lookup fails at the moment of
// the delete.
type removeRecordOnFirstRead struct {
	recordPath string
	err        error
}

func (r *removeRecordOnFirstRead) Read(p []byte) (int, error) {
	if err := os.Remove(r.recordPath); err != nil {
		r.err = err
		return 0, err
	}
	return copy(p, "y\n"), io.EOF
}

// TestRunsPruneRecordVanishedDuringPromptRefused asserts the gate's
// failure arm: the listed record is gone when the delete is about to
// happen, and prune says so — exit 9, the refusal on stderr, no
// deleted line — instead of pretending the run was deleted. Without
// the gate the remove itself would fail with a raw "no such file"
// error; the gate turns the vanished name into the same refusal
// family as the other gates.
func TestRunsPruneRecordVanishedDuringPromptRefused(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	withStdinIsTerminal(t, true)
	const runID = "20260830T101500Z-1"
	recordPath := writeListRecord(t, dir, runID, deadPID, "2026-08-30T10:15:00Z", "/ws-a", 1, true, "completed", "")

	stdin := &removeRecordOnFirstRead{recordPath: recordPath}
	var stdout, stderr bytes.Buffer
	code := mainWithContext(context.Background(), []string{"runs", "prune", "--keep", "0"}, stdin, &stdout, &stderr)
	if stdin.err != nil {
		t.Fatalf("removal during the prompt failed: %v", stdin.err)
	}
	if code != 9 {
		t.Fatalf("prune with the record vanished during the prompt = (%d, %q, %q), want exit 9", code, stdout.String(), stderr.String())
	}
	if _, err := os.Lstat(recordPath); !os.IsNotExist(err) {
		t.Fatalf("%s exists after the prune: %v", recordPath, err)
	}
	if want := "kept 0 newest\n"; stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
	if !strings.Contains(stderr.String(), "refusing to delete") || !strings.Contains(stderr.String(), "cannot be examined") {
		t.Fatalf("stderr = %q, want the refusal for the vanished record", stderr.String())
	}
}

// createRecordOnFirstRead writes a record file on the first stdin
// Read, then answers "y": the name is empty when the candidate is
// chosen and holds a freshly written file exactly while the question
// is on screen — the mirror of removeRecordOnFirstRead in the same
// window.
type createRecordOnFirstRead struct {
	recordPath string
	err        error
}

func (c *createRecordOnFirstRead) Read(p []byte) (int, error) {
	if err := os.WriteFile(c.recordPath, []byte("{\"schemaVersion\":1}\n"), 0o600); err != nil {
		c.err = err
		return 0, err
	}
	return copy(p, "y\n"), io.EOF
}

// TestRunsPruneRecordAppearsDuringPromptRefused asserts the gate's
// no-identity arm: a candidate whose file could not be looked up when
// it was chosen — here a path that does not exist, injected through
// the list seam — keeps nothing, so the delete cannot anchor on
// anything, and a file that appears under the name while the question
// is on screen is refused even though it is a regular file: nothing
// about that file was ever shown, the delete says so with the
// identity refusal, exit 9, and the file stays. The selection-time
// lookup failure invents no new arm — the not-listed file gets the
// same refusal every not-listed file gets.
func TestRunsPruneRecordAppearsDuringPromptRefused(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	withStdinIsTerminal(t, true)
	const runID = "20260830T101500Z-1"
	recordPath := filepath.Join(dir, runID+".jsonl")
	original := runlogList
	runlogList = func(string) ([]runlog.Run, error) {
		return []runlog.Run{{RunID: runID, Outcome: "completed", Path: recordPath}}, nil
	}
	t.Cleanup(func() { runlogList = original })

	stdin := &createRecordOnFirstRead{recordPath: recordPath}
	var stdout, stderr bytes.Buffer
	code := mainWithContext(context.Background(), []string{"runs", "prune", "--keep", "0"}, stdin, &stdout, &stderr)
	if stdin.err != nil {
		t.Fatalf("creation during the prompt failed: %v", stdin.err)
	}
	if code != 9 {
		t.Fatalf("prune with the record appearing during the prompt = (%d, %q, %q), want exit 9", code, stdout.String(), stderr.String())
	}
	if want := "kept 0 newest\n"; stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
	if !strings.Contains(stderr.String(), "no longer the record that was listed") {
		t.Fatalf("stderr = %q, want the identity refusal naming the never-listed file", stderr.String())
	}
	if _, err := os.Stat(recordPath); err != nil {
		t.Fatalf("the file that appeared during the prompt was deleted: %v", err)
	}
}

// touchRecordOnFirstRead refreshes the record file's modification
// time on the first stdin Read, then answers "y": the one fact the
// freshness check reads changes exactly while the question is on
// screen, the way a writer appending to the record would move it.
type touchRecordOnFirstRead struct {
	recordPath string
	err        error
}

func (r *touchRecordOnFirstRead) Read(p []byte) (int, error) {
	now := time.Now()
	if err := os.Chtimes(r.recordPath, now, now); err != nil {
		r.err = err
		return 0, err
	}
	return copy(p, "y\n"), io.EOF
}

// TestRunsPruneTouchedUnknownRecordKeptAtDeleteTime asserts the
// grace-window re-check at the moment of the delete: an unknown
// record selected while stale is touched during the prompt — the
// record is being written right now, after selection already looked —
// and the answer is y. Prune spares it and reports it as kept — the
// kept line, and the summary counts it in the unreadable clause, the
// same reporting a selection-time spare gets, never the still-running
// clause — and exits 0: sparing is not a failure. A record with a
// real outcome would go whatever its modification time, but this
// record's outcome is unknown, so the freshness question is the one
// that decides.
func TestRunsPruneTouchedUnknownRecordKeptAtDeleteTime(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	withStdinIsTerminal(t, true)
	const runID = "20260831T120000Z-1"
	recordPath := filepath.Join(dir, runID+".jsonl")
	if err := os.WriteFile(recordPath, []byte("not json at all\n"), 0o600); err != nil {
		t.Fatalf("write unknown record: %v", err)
	}
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(recordPath, stale, stale); err != nil {
		t.Fatalf("age the record: %v", err)
	}

	stdin := &touchRecordOnFirstRead{recordPath: recordPath}
	var stdout, stderr bytes.Buffer
	code := mainWithContext(context.Background(), []string{"runs", "prune", "--keep", "0"}, stdin, &stdout, &stderr)
	if stdin.err != nil {
		t.Fatalf("touch during the prompt failed: %v", stdin.err)
	}
	want := "kept " + runID + "\nkept 0 newest, 1 unreadable\n"
	if code != 0 || stdout.String() != want {
		t.Fatalf("prune with the record touched during the prompt = (%d, %q, %q), want (0, %q)", code, stdout.String(), stderr.String(), want)
	}
	if _, err := os.Stat(recordPath); err != nil {
		t.Fatalf("the touched unknown record %s was deleted: %v", recordPath, err)
	}
}

// TestRecordRecentlyModifiedUnstatablePathSpares asserts the
// fail-safe direction of the freshness lookup: a path that cannot be
// looked up — here, a name that does not exist — reports fresh, so
// selection spares rather than deletes. Every doubtful case in this
// reader resolves toward silence, never toward acting.
func TestRecordRecentlyModifiedUnstatablePathSpares(t *testing.T) {
	dir := t.TempDir()
	if !recordRecentlyModified(filepath.Join(dir, "missing.jsonl")) {
		t.Fatalf("recordRecentlyModified on an unstatable path = false, want true: a record that cannot be examined must be spared, not deleted")
	}
}

// TestRunsPruneUnstatableUnknownRecordReportedUnreadable asserts the
// reporting side of TestRecordRecentlyModifiedUnstatablePathSpares
// end to end: an unknown record whose timestamp cannot be read —
// here a path that does not exist, injected through the list seam
// exactly as the delete-failure tests inject theirs — is kept at
// selection, its id reaches keptUnreadable in the --json document,
// and the human clause says only `unreadable`. Nobody measured when
// that record changed — its timestamp was never read at all — so
// neither report may claim it changed recently.
func TestRunsPruneUnstatableUnknownRecordReportedUnreadable(t *testing.T) {
	// One fresh records directory per subtest: a prune that deleted
	// the completed record in one arm must not change what the other
	// arm lists.
	withRecords := func(t *testing.T) (missingPath, realPath string) {
		t.Helper()
		dir := t.TempDir()
		withRunlogDir(t, dir)
		// The unknown record's path cannot be looked up: it does not
		// exist. recordRecentlyModified treats a failed lookup as
		// fresh, so selection keeps the record; the report may not
		// claim to know when it changed. A second, deletable record
		// makes the summary line render at all.
		missingPath = filepath.Join(dir, "20260830T101500Z-0.jsonl")
		realPath = writeListRecord(t, dir, "20260830T103000Z-1", deadPID, "2026-08-30T10:30:00Z", "/ws-a", 1, true, "completed", "")
		original := runlogList
		runlogList = func(string) ([]runlog.Run, error) {
			return []runlog.Run{
				{RunID: "20260830T101500Z-0", Outcome: "unknown", Path: missingPath},
				{RunID: "20260830T103000Z-1", Outcome: "completed", Path: realPath},
			}, nil
		}
		t.Cleanup(func() { runlogList = original })
		return missingPath, realPath
	}

	t.Run("human clause", func(t *testing.T) {
		_, realPath := withRecords(t)
		code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0", "--yes"}, "")
		if code != 0 || stderr != "" {
			t.Fatalf("runs prune = (%d, %q, %q), want exit 0 with empty stderr", code, stdout, stderr)
		}
		const want = "deleted 20260830T103000Z-1\nkept 0 newest, 1 unreadable\n"
		if stdout != want {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
		if _, err := os.Stat(realPath); !os.IsNotExist(err) {
			t.Fatalf("record %s still exists after the prune: %v", realPath, err)
		}
	})

	t.Run("--json document", func(t *testing.T) {
		_, realPath := withRecords(t)
		code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0", "--yes", "--json"}, "")
		if code != 0 || stderr != "" {
			t.Fatalf("runs prune --json = (%d, %q, %q), want exit 0 with empty stderr", code, stdout, stderr)
		}
		var document struct {
			SchemaVersion  int      `json:"schemaVersion"`
			Deleted        []string `json:"deleted"`
			KeptNewest     int      `json:"keptNewest"`
			KeptRunning    []string `json:"keptRunning"`
			KeptUnreadable []string `json:"keptUnreadable"`
		}
		if err := json.Unmarshal([]byte(stdout), &document); err != nil {
			t.Fatalf("decode document %q: %v", stdout, err)
		}
		if document.SchemaVersion != 1 || len(document.Deleted) != 1 || document.Deleted[0] != "20260830T103000Z-1" || document.KeptNewest != 0 || len(document.KeptRunning) != 0 || len(document.KeptUnreadable) != 1 || document.KeptUnreadable[0] != "20260830T101500Z-0" {
			t.Fatalf("document = %#v, want the unstatable unknown record in keptUnreadable and the completed one in deleted", document)
		}
		if _, err := os.Stat(realPath); !os.IsNotExist(err) {
			t.Fatalf("record %s still exists after the prune: %v", realPath, err)
		}
	})
}

// TestRunsPruneSymlinkFreshnessByLinkNotTarget pins the two facts a
// symlink record exercises: freshness is decided by the link's own
// timestamp, never by the target's, and a stale symlink that reaches
// the delete is removed — the link itself, never whatever it points
// at. The construction is the mirror of the one in the finding — a
// freshly created link whose target is two hours old: os.Stat would
// follow the link to the old target and read stale, while the link's
// own timestamp, the one that matters, is fresh.
func TestRunsPruneSymlinkFreshnessByLinkNotTarget(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)

	targetDir := t.TempDir()
	targetPath := filepath.Join(targetDir, "old.jsonl")
	if err := os.WriteFile(targetPath, []byte("not a record\n"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(targetPath, stale, stale); err != nil {
		t.Fatalf("age the target: %v", err)
	}
	linkPath := filepath.Join(dir, "20260831T120000Z-1.jsonl")
	if err := os.Symlink(targetPath, linkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// The freshness fact: the link is freshly created, so its own
	// timestamp is fresh even though its target is old.
	if !recordRecentlyModified(linkPath) {
		t.Fatalf("recordRecentlyModified(%q) = false, want true: the target's age must not answer for the link", linkPath)
	}

	// End to end through the real listing, the same fresh link: the
	// unknown record is spared whole — nothing to prune, exit 0 — and
	// both the link and its target still exist.
	code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0", "--yes"}, "")
	if code != 0 || stdout != "nothing to prune\n" || stderr != "" {
		t.Fatalf("prune with the fresh symlink record = (%d, %q, %q), want (0, \"nothing to prune\\n\", \"\")", code, stdout, stderr)
	}
	if info, err := os.Lstat(linkPath); err != nil {
		t.Fatalf("the symlink %s is gone: %v", linkPath, err)
	} else if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink after the prune", linkPath)
	}
	if _, err := os.Stat(targetPath); err != nil {
		t.Fatalf("the target %s is gone: %v", targetPath, err)
	}

	// The deletion fact: a symlink record that is selected — injected
	// through the list seam with the same real path, the way the
	// delete-failure tests do — is deleted like any other unreadable
	// record: the link is removed, exit 0, and the target is never
	// touched. A gate that refused the link by its type would leave
	// this junk in place forever, and a removal that followed the
	// link would destroy the target.
	original := runlogList
	runlogList = func(string) ([]runlog.Run, error) {
		return []runlog.Run{{RunID: "20260831T120000Z-1", Outcome: "completed", Path: linkPath}}, nil
	}
	t.Cleanup(func() { runlogList = original })
	code, stdout, stderr = runCLI(t, []string{"runs", "prune", "--keep", "0", "--yes"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("prune of the selected symlink record = (%d, %q, %q), want exit 0 with empty stderr", code, stdout, stderr)
	}
	if want := "deleted 20260831T120000Z-1\nkept 0 newest\n"; stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	if _, err := os.Lstat(linkPath); !os.IsNotExist(err) {
		t.Fatalf("the symlink %s was not deleted: %v", linkPath, err)
	}
	if _, err := os.Stat(targetPath); err != nil {
		t.Fatalf("the target %s was touched: %v", targetPath, err)
	}
}

// TestRunsPruneSelectedDirectoryRefusedByType asserts the type
// gate's refusal side: a candidate whose file is a directory — a
// name that was a directory when the candidate was chosen and is the
// same directory at the delete, so the identity check passes — is
// refused by what it is: exit 9, the refusal on stderr, the
// directory still there, and no deleted line for its id. The
// candidate is injected through the list seam with a real directory
// inside the records directory, the only way a directory can reach
// the delete list at all: the reader never lists one.
func TestRunsPruneSelectedDirectoryRefusedByType(t *testing.T) {
	dir := t.TempDir()
	withRunlogDir(t, dir)
	realPath := writeListRecord(t, dir, "20260830T103000Z-1", deadPID, "2026-08-30T10:30:00Z", "/ws-a", 1, true, "completed", "")
	dirPath := filepath.Join(dir, "20260830T101500Z-0.jsonl")
	if err := os.Mkdir(dirPath, 0o700); err != nil {
		t.Fatalf("mkdir the directory record: %v", err)
	}
	original := runlogList
	runlogList = func(string) ([]runlog.Run, error) {
		return []runlog.Run{
			{RunID: "20260830T101500Z-0", Outcome: "completed", Path: dirPath},
			{RunID: "20260830T103000Z-1", Outcome: "completed", Path: realPath},
		}, nil
	}
	t.Cleanup(func() { runlogList = original })

	code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0", "--yes"}, "")
	if code != 9 {
		t.Fatalf("prune of the selected directory record = (%d, %q, %q), want exit 9", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "deleted 20260830T103000Z-1\n") {
		t.Fatalf("stdout = %q, want the deleted line for the record that went", stdout)
	}
	if !strings.Contains(stdout, "kept 0 newest\n") {
		t.Fatalf("stdout = %q, want the summary line", stdout)
	}
	if !strings.Contains(stderr, "not a regular file or symlink") {
		t.Fatalf("stderr = %q, want the type refusal", stderr)
	}
	if info, err := os.Lstat(dirPath); err != nil {
		t.Fatalf("the directory %s was removed: %v", dirPath, err)
	} else if !info.IsDir() {
		t.Fatalf("%s is not a directory after the prune", dirPath)
	}
	if _, err := os.Stat(realPath); !os.IsNotExist(err) {
		t.Fatalf("in-directory record %s still exists: %v", realPath, err)
	}
}

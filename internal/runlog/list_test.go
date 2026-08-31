package runlog

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// writeListRecord writes one hand-built record file into dir: a start
// line carrying the pid, started-at, workspace, and task count the
// test chose, and — when withFinish is true — a finish line carrying
// either a result with the outcome resultOutcome or, when
// resultOutcome is empty, the error text errorText, matching the
// writer's exactly-one-of-the-two invariant. The pid is chosen by the
// test, never read back out of a record, so the liveness script's
// answers stay independent of what the record carries.
func writeListRecord(t *testing.T, dir, runID string, pid int, startedAt, workspace string, tasks int, withFinish bool, resultOutcome, errorText string) string {
	t.Helper()
	start := map[string]any{
		"schemaVersion": schemaVersion,
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
			"schemaVersion": schemaVersion,
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

// TestListClassifiesFinishedRecordsFromTheFinishLine asserts the
// finished arm of the outcome decision: a finish line carrying a
// result yields the result's outcome copy-verbatim — never renamed —
// and a finish line carrying the error arm instead yields "error",
// whether the error text decodes or the finish line carries no result
// at all, the record format making the two arms mutually exclusive.
func TestListClassifiesFinishedRecordsFromTheFinishLine(t *testing.T) {
	dir := t.TempDir()
	// A finished record whose result carries a product outcome...
	resultPath := writeListRecord(t, dir, "20260830T101500Z-1", 4242, "1980-01-02T03:04:05Z", "/workspace-a", 2, true, "undeclared-writes", "")
	// ...and a finished record whose start line carries no task array.
	zeroTasksPath := writeListRecord(t, dir, "20260830T102000Z-2", 4242, "2026-08-30T10:20:00Z", "/workspace-b", 0, true, "completed", "")
	// A finish line without a result is the error arm, exactly as the
	// writer writes an erroring run; writeRecord's finish line carries
	// no result, the minimal shape the interrupted-run tests use.
	errorPath := writeRecord(t, dir, "20260830T103000Z-3", 4242, true)

	runs, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []Run{
		{RunID: "20260830T103000Z-3", StartedAt: "2026-08-30T10:15:00Z", Workspace: "/workspace", Tasks: 0, Outcome: "error", Path: errorPath},
		{RunID: "20260830T102000Z-2", StartedAt: "2026-08-30T10:20:00Z", Workspace: "/workspace-b", Tasks: 0, Outcome: "completed", Path: zeroTasksPath},
		{RunID: "20260830T101500Z-1", StartedAt: "1980-01-02T03:04:05Z", Workspace: "/workspace-a", Tasks: 2, Outcome: "undeclared-writes", Path: resultPath},
	}
	if !slices.Equal(runs, want) {
		t.Fatalf("runs = %#v, want %#v", runs, want)
	}
}

// TestListClassifiesRunningAndInterruptedThroughTheLivenessSeam
// asserts the unfinished arm: no finish line, and the start line's
// process — scripted through the same pidAlive seam the
// interrupted-run scan uses — decides running from interrupted. This
// is the guard the liveness seam protects: were the classifier to stop
// consulting it, every unfinished record would read as one of the two
// without evidence.
func TestListClassifiesRunningAndInterruptedThroughTheLivenessSeam(t *testing.T) {
	withPidAlive(t, func(pid int32) (bool, error) { return pid == 4242, nil })
	dir := t.TempDir()
	runningPath := writeListRecord(t, dir, "20260830T101500Z-1", 4242, "2026-08-30T10:15:00Z", "/workspace-a", 1, false, "", "")
	interruptedPath := writeListRecord(t, dir, "20260830T102000Z-2", 4243, "2026-08-30T10:20:00Z", "/workspace-b", 3, false, "", "")

	runs, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []Run{
		{RunID: "20260830T102000Z-2", StartedAt: "2026-08-30T10:20:00Z", Workspace: "/workspace-b", Tasks: 3, Outcome: "interrupted", Path: interruptedPath},
		{RunID: "20260830T101500Z-1", StartedAt: "2026-08-30T10:15:00Z", Workspace: "/workspace-a", Tasks: 1, Outcome: "running", Path: runningPath},
	}
	if !slices.Equal(runs, want) {
		t.Fatalf("runs = %#v, want %#v", runs, want)
	}
}

// TestListLivenessErrorMeansRunning asserts a liveness error counts as
// alive, the same fail-safe direction as the interrupted-run warning:
// doubtful cases resolve toward a live run, never toward a wrong
// accusation of interruption.
func TestListLivenessErrorMeansRunning(t *testing.T) {
	withPidAlive(t, func(pid int32) (bool, error) {
		if pid == 4242 {
			return false, errors.New("liveness unavailable")
		}
		return false, nil
	})
	dir := t.TempDir()
	path := writeListRecord(t, dir, "20260830T101500Z-1", 4242, "2026-08-30T10:15:00Z", "/workspace", 0, false, "", "")

	runs, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(runs) != 1 || runs[0].Outcome != "running" || runs[0].Path != path {
		t.Fatalf("runs = %#v, want one running record at %s", runs, path)
	}
}

// TestListTornLastLineClassifiedByStartLine asserts a record being
// written right now — a valid start line and a partial last line —
// is classified normally by its start line: a partial finish line is
// no finish line, so the liveness seam decides, and a live run never
// looks interrupted because its record ends mid-line.
func TestListTornLastLineClassifiedByStartLine(t *testing.T) {
	writeTorn := func(t *testing.T, dir, runID string, pid int) string {
		t.Helper()
		start := "{\"schemaVersion\":1,\"event\":\"start\",\"runId\":\"" + runID + "\",\"startedAt\":\"2026-08-30T10:15:00Z\",\"workspace\":\"/workspace\",\"pid\":" + strconv.Itoa(pid) + ",\"tasks\":[]}\n"
		// The finish line is cut mid-write, as a kill leaves it.
		torn := "{\"schemaVersion\":1,\"event\":\"finish\",\"runId\":\"" + runID
		path := filepath.Join(dir, runID+".jsonl")
		if err := os.WriteFile(path, []byte(start+torn), 0o600); err != nil {
			t.Fatalf("write torn record: %v", err)
		}
		return path
	}

	dir := t.TempDir()
	withPidAlive(t, func(pid int32) (bool, error) { return pid == 4242, nil })
	livePath := writeTorn(t, dir, "20260830T101500Z-1", 4242)
	deadPath := writeTorn(t, dir, "20260830T102000Z-2", 4243)

	runs, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []Run{
		{RunID: "20260830T102000Z-2", StartedAt: "2026-08-30T10:15:00Z", Workspace: "/workspace", Tasks: 0, Outcome: "interrupted", Path: deadPath},
		{RunID: "20260830T101500Z-1", StartedAt: "2026-08-30T10:15:00Z", Workspace: "/workspace", Tasks: 0, Outcome: "running", Path: livePath},
	}
	if !slices.Equal(runs, want) {
		t.Fatalf("runs = %#v, want %#v", runs, want)
	}
}

// TestListUnknownRecordsStillListed asserts a record with no usable
// start line is listed anyway, as unknown: the run id from the file
// name and the path are kept, the fields the record could not supply
// are empty strings and zero. Hiding an entry the list cannot read
// would be worse than showing it with what the file name tells.
func TestListUnknownRecordsStillListed(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"20260830T100000Z-0.jsonl":  "not json at all\n",
		"20260830T101000Z-1.jsonl":  "",
		"20260830T100500Z-0a.jsonl": "{\"schemaVersion\":1,\"event\":\"worker\",\"pid\":4242}\n",
		"20260830T101500Z-0b.jsonl": "{\"schemaVersion\":1,\"event\":\"start\"}\n",
		"20260830T102000Z-0c.jsonl": "{\"schemaVersion\":1,\"event\":\"finish\",\"runId\":\"20260830T102000Z-0c\"}\n",
		"20260830T102500Z-0d.jsonl": "{\"schemaVersion\":1,\"event\":\"start\",\"pid\":0}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write record %s: %v", name, err)
		}
	}

	runs, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []Run{
		{RunID: "20260830T102500Z-0d", Outcome: "unknown", Path: filepath.Join(dir, "20260830T102500Z-0d.jsonl")},
		{RunID: "20260830T102000Z-0c", Outcome: "unknown", Path: filepath.Join(dir, "20260830T102000Z-0c.jsonl")},
		{RunID: "20260830T101500Z-0b", Outcome: "unknown", Path: filepath.Join(dir, "20260830T101500Z-0b.jsonl")},
		{RunID: "20260830T101000Z-1", Outcome: "unknown", Path: filepath.Join(dir, "20260830T101000Z-1.jsonl")},
		{RunID: "20260830T100500Z-0a", Outcome: "unknown", Path: filepath.Join(dir, "20260830T100500Z-0a.jsonl")},
		{RunID: "20260830T100000Z-0", Outcome: "unknown", Path: filepath.Join(dir, "20260830T100000Z-0.jsonl")},
	}
	if !slices.Equal(runs, want) {
		t.Fatalf("runs = %#v, want %#v", runs, want)
	}
}

// TestListNewestFirst asserts the returned order is newest first, the
// run ids sorted as plain strings descending — chronological because a
// run id begins with a fixed-width UTC timestamp — never by a parsed
// time.
func TestListNewestFirst(t *testing.T) {
	withPidAlive(t, func(pid int32) (bool, error) { return pid == 4242, nil })
	dir := t.TempDir()
	oldPath := writeListRecord(t, dir, "20260830T101500Z-1", 4243, "2026-08-30T10:15:00Z", "/workspace-a", 0, false, "", "")
	newestPath := writeListRecord(t, dir, "20260830T103000Z-3", 4243, "2026-08-30T10:30:00Z", "/workspace-b", 0, false, "", "")
	middlePath := writeListRecord(t, dir, "20260830T102000Z-2", 4243, "2026-08-30T10:20:00Z", "/workspace-c", 0, false, "", "")

	runs, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var got []string
	for _, run := range runs {
		got = append(got, run.RunID)
	}
	if want := []string{"20260830T103000Z-3", "20260830T102000Z-2", "20260830T101500Z-1"}; !slices.Equal(got, want) {
		t.Fatalf("run order = %v, want %v", got, want)
	}
	if runs[0].Path != newestPath || runs[1].Path != middlePath || runs[2].Path != oldPath {
		t.Fatalf("paths = %v, %v, %v; want newest first", runs[0].Path, runs[1].Path, runs[2].Path)
	}
}

// TestListMissingAndEmptyRecordsDirAreNotErrors asserts a missing
// records directory and an empty one both list as no runs with no
// error, and that the empty result is a non-nil slice so a JSON
// document renders it as [] and never null.
func TestListMissingAndEmptyRecordsDirAreNotErrors(t *testing.T) {
	if runs, err := List(filepath.Join(t.TempDir(), "missing")); err != nil || runs == nil || len(runs) != 0 {
		t.Fatalf("List(missing) = (%#v, %v), want ([], nil)", runs, err)
	}
	if runs, err := List(t.TempDir()); err != nil || runs == nil || len(runs) != 0 {
		t.Fatalf("List(empty) = (%#v, %v), want ([], nil)", runs, err)
	}
}

// TestListIgnoresNonRecordFilesAndWritesNothing asserts the only
// *.jsonl files are records — reported.json, a left-behind
// .reported.json.tmp-* stage, a subdirectory, and any other file are
// skipped silently — and that the list itself writes nothing: the
// marker file it deliberately must not touch is byte-identical after
// the scan, and no new file appeared.
func TestListIgnoresNonRecordFilesAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	markerContent := "{\"schemaVersion\":1,\"watermark\":\"20260830T101500Z-1\"}\n"
	if err := os.WriteFile(filepath.Join(dir, markerFileName), []byte(markerContent), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".reported.json.tmp-123"), []byte("stale stage"), 0o600); err != nil {
		t.Fatalf("write temp stage: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a record"), 0o600); err != nil {
		t.Fatalf("write notes: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o700); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "subdir", "20260830T100000Z-9.jsonl"), []byte("{\"schemaVersion\":1,\"event\":\"start\",\"pid\":4242}\n"), 0o600); err != nil {
		t.Fatalf("write nested record: %v", err)
	}
	recordPath := writeListRecord(t, dir, "20260830T101500Z-1", 4242, "2026-08-30T10:15:00Z", "/workspace", 1, false, "", "")

	result, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(result) != 1 || result[0].RunID != "20260830T101500Z-1" || result[0].Path != recordPath {
		t.Fatalf("runs = %#v, want only the one record at %s", result, recordPath)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("directory entries = %d, want 5 (the record, the marker, the temp stage, the notes, the subdir)", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(dir, markerFileName))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if string(data) != markerContent {
		t.Fatalf("marker changed by the list: %q, want %q", data, markerContent)
	}
}

// TestListUnreadableRecordsDirReturnsError asserts a records directory
// that exists but cannot be read returns the error and no runs, so the
// CLI exits 9.
func TestListUnreadableRecordsDirReturnsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not enforced on Windows")
	}
	dir := t.TempDir()
	writeListRecord(t, dir, "20260830T101500Z-1", 4242, "2026-08-30T10:15:00Z", "/workspace", 0, false, "", "")
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	runs, err := List(dir)
	if err == nil || len(runs) != 0 {
		t.Fatalf("List(unreadable dir) = (%#v, %v), want (nil, error)", runs, err)
	}
}

// TestListDisplayDamageDoesNotChangeClassification asserts the
// classification is decided by the same minimal facts the
// interrupted-run scan uses: a start line whose display field is
// malformed — startedAt as a number here — is still a usable start
// line, so the record is classified by liveness normally and only the
// display fields it could not carry stay empty. The two readers must
// never disagree about a record because one of them decoded more of
// it.
func TestListDisplayDamageDoesNotChangeClassification(t *testing.T) {
	withPidAlive(t, func(pid int32) (bool, error) { return true, nil })
	dir := t.TempDir()
	path := filepath.Join(dir, "20260830T101500Z-1.jsonl")
	if err := os.WriteFile(path, []byte("{\"schemaVersion\":1,\"event\":\"start\",\"runId\":\"20260830T101500Z-1\",\"startedAt\":42,\"workspace\":\"/workspace\",\"pid\":4242,\"tasks\":[1,2]}\n"), 0o600); err != nil {
		t.Fatalf("write record: %v", err)
	}

	runs, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []Run{{RunID: "20260830T101500Z-1", Outcome: "running", Path: path}}
	if !slices.Equal(runs, want) {
		t.Fatalf("runs = %#v, want %#v", runs, want)
	}
}

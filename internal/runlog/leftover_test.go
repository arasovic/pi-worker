package runlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// workerSpec is one worker line of a test record: the pid the worker
// launched and, when nonzero, its creation time. A zero creation time
// writes the line without the createTime key — the exact shape of
// every record written before the field existed.
type workerSpec struct {
	pid        int
	createTime int64
}

// writeLeftoverRecord writes one hand-built record file into dir: a
// start line carrying the start pid the test chose, the given worker
// lines in order, and — when finished is true — a finish line. Pids
// and creation times are chosen by the test, never read back out of a
// record, so the process-table script's answers stay independent of
// what the record carries.
func writeLeftoverRecord(t *testing.T, dir, runID string, startPID int, finished bool, workers ...workerSpec) string {
	t.Helper()
	lines := []map[string]any{
		{
			"schemaVersion": schemaVersion,
			"event":         "start",
			"runId":         runID,
			"startedAt":     "2026-08-30T10:15:00Z",
			"workspace":     "/workspace",
			"pid":           startPID,
			"tasks":         []any{},
		},
	}
	for i, w := range workers {
		line := map[string]any{
			"schemaVersion": schemaVersion,
			"event":         "worker",
			"runId":         runID,
			"at":            "2026-08-30T10:15:00Z",
			"workerId":      i + 1,
			"pid":           w.pid,
		}
		if w.createTime != 0 {
			line["createTime"] = w.createTime
		}
		lines = append(lines, line)
	}
	if finished {
		lines = append(lines, map[string]any{
			"schemaVersion": schemaVersion,
			"event":         "finish",
			"runId":         runID,
			"finishedAt":    "2026-08-30T10:15:30Z",
		})
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

// withLiveProcesses replaces the process-table seam for the duration
// of one test with the scripted rows and error, and returns a pointer
// to a call counter so a test can assert the sweep stayed untouched.
// Scripted like the other seams in this repo and restored with
// t.Cleanup.
func withLiveProcesses(t *testing.T, rows []liveProcess, err error) *int {
	t.Helper()
	calls := 0
	original := liveProcesses
	liveProcesses = func() ([]liveProcess, error) {
		calls++
		return rows, err
	}
	t.Cleanup(func() { liveProcesses = original })
	return &calls
}

// snapshotDir returns every file's path and bytes under dir, the
// before-and-after shape a reader that must write nothing is compared
// against.
func snapshotDir(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	snapshot := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		snapshot[path] = data
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", dir, err)
	}
	return snapshot
}

// TestLeftoversReportsSettledRunsWorkerGroupMembers asserts a settled
// run — here by its finish line, so the liveness seam is never
// consulted — whose worker group holds two live members reports both,
// sorted ascending with the scripted duplicate removed, under the
// run's id and path. This is the guard the process-table seam
// protects: with liveProcesses answering nothing, the report below
// must be empty.
func TestLeftoversReportsSettledRunsWorkerGroupMembers(t *testing.T) {
	withLiveProcesses(t, []liveProcess{
		{pid: 5011, pgid: 5001, createTime: 1200},
		{pid: 5010, pgid: 5001, createTime: 1500},
		{pid: 5010, pgid: 5001, createTime: 1500},
	}, nil)
	withPidAlive(t, func(pid int32) (bool, error) {
		t.Fatalf("pidAlive consulted for pid %d of a finished record", pid)
		return false, nil
	})
	dir := t.TempDir()
	path := writeLeftoverRecord(t, dir, "20260830T101500Z-1", 4242, true, workerSpec{pid: 5001, createTime: 1000})

	leftovers, err := Leftovers(dir)
	if err != nil {
		t.Fatalf("Leftovers: %v", err)
	}
	want := []Leftover{{RunID: "20260830T101500Z-1", Path: path, PIDs: []int{5010, 5011}}}
	if !reflect.DeepEqual(leftovers, want) {
		t.Fatalf("leftovers = %#v, want %#v", leftovers, want)
	}
}

// TestLeftoversReportsTheLiveWorkerItself asserts the positive arm of
// the identity check: when the recorded pid is still alive with the
// recorded creation time, it is still the worker, its group is
// genuinely the worker's, and the worker itself — a live process
// carrying its own number as its group — is reported like any other
// group member.
func TestLeftoversReportsTheLiveWorkerItself(t *testing.T) {
	withLiveProcesses(t, []liveProcess{
		{pid: 5001, pgid: 5001, createTime: 1000},
		{pid: 5011, pgid: 5001, createTime: 1500},
	}, nil)
	dir := t.TempDir()
	path := writeLeftoverRecord(t, dir, "20260830T101500Z-1", 4242, true, workerSpec{pid: 5001, createTime: 1000})

	leftovers, err := Leftovers(dir)
	if err != nil {
		t.Fatalf("Leftovers: %v", err)
	}
	want := []Leftover{{RunID: "20260830T101500Z-1", Path: path, PIDs: []int{5001, 5011}}}
	if !reflect.DeepEqual(leftovers, want) {
		t.Fatalf("leftovers = %#v, want %#v", leftovers, want)
	}
}

// TestLeftoversSkipsWorkerWhoseRowCannotBeRead asserts an unreadable
// process-table row does not establish the recorded worker's identity.
// Another row in that group must not be reported while that identity is
// unconfirmed.
func TestLeftoversSkipsWorkerWhoseRowCannotBeRead(t *testing.T) {
	withLiveProcesses(t, []liveProcess{
		{pid: 5001, pgid: 5001, createTime: 1000, unreadable: true},
		{pid: 5010, pgid: 5001, createTime: 1100},
	}, nil)
	dir := t.TempDir()
	writeLeftoverRecord(t, dir, "20260830T101500Z-1", 4242, true, workerSpec{pid: 5001, createTime: 1000})

	leftovers, err := Leftovers(dir)
	if err != nil {
		t.Fatalf("Leftovers: %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("leftovers = %#v, want none", leftovers)
	}
}

func TestLeftoversSweepsWhenWorkerIsAbsent(t *testing.T) {
	withLiveProcesses(t, []liveProcess{
		{pid: 5010, pgid: 5001, createTime: 1100},
	}, nil)
	dir := t.TempDir()
	path := writeLeftoverRecord(t, dir, "20260830T101500Z-1", 4242, true, workerSpec{pid: 5001, createTime: 1000})

	leftovers, err := Leftovers(dir)
	if err != nil {
		t.Fatalf("Leftovers: %v", err)
	}
	want := []Leftover{{RunID: "20260830T101500Z-1", Path: path, PIDs: []int{5010}}}
	if !reflect.DeepEqual(leftovers, want) {
		t.Fatalf("leftovers = %#v, want %#v", leftovers, want)
	}
}

func TestLeftoversSkipsGroupWhosePidWasReused(t *testing.T) {
	withLiveProcesses(t, []liveProcess{
		// The recorded pid 5001 now belongs to an unrelated process in
		// group 5099, and the worker's old group still has a survivor.
		{pid: 5001, pgid: 5099, createTime: 9999},
		{pid: 5010, pgid: 5001, createTime: 1100},
	}, nil)
	dir := t.TempDir()
	writeLeftoverRecord(t, dir, "20260830T101500Z-1", 4242, true, workerSpec{pid: 5001, createTime: 1000})

	leftovers, err := Leftovers(dir)
	if err != nil {
		t.Fatalf("Leftovers: %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("leftovers = %#v, want none", leftovers)
	}
}

// TestLeftoversSkipsWorkerWithoutCreateTime asserts a worker line
// without a creation time is never reportable, not even when live
// processes carry its group number: the identity pair is incomplete,
// so the number alone cannot name a group. The scripted group would
// match, which is what makes the skip observable — and the lazy sweep
// means the process table is never consulted at all. This is the guard
// the §5.2 skip protects: the two records already on this machine are
// exactly this class, and a guard with no test has shipped once
// already.
func TestLeftoversSkipsWorkerWithoutCreateTime(t *testing.T) {
	calls := withLiveProcesses(t, []liveProcess{
		{pid: 5010, pgid: 5001, createTime: 1100},
	}, nil)
	dir := t.TempDir()
	writeLeftoverRecord(t, dir, "20260830T101500Z-1", 4242, true, workerSpec{pid: 5001})

	leftovers, err := Leftovers(dir)
	if err != nil {
		t.Fatalf("Leftovers: %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("leftovers = %#v, want none", leftovers)
	}
	if *calls != 0 {
		t.Fatalf("liveProcesses called %d times, want 0", *calls)
	}
}

// TestLeftoversSkipsRunStillInFlight asserts a run that is still
// going has not left anything behind: no finish line and its own pid
// still alive — decided through the same liveness seam the
// interrupted-run reader uses — means the record is skipped even
// though its worker groups are fully alive in the scripted table.
// This is the guard the settled test protects: were the settled check
// dropped, the live run below would be reported and this test fails.
func TestLeftoversSkipsRunStillInFlight(t *testing.T) {
	calls := withLiveProcesses(t, []liveProcess{
		{pid: 5001, pgid: 5001, createTime: 1000},
		{pid: 5011, pgid: 5001, createTime: 1500},
	}, nil)
	withPidAlive(t, func(pid int32) (bool, error) { return pid == 4242, nil })
	dir := t.TempDir()
	writeLeftoverRecord(t, dir, "20260830T101500Z-1", 4242, false, workerSpec{pid: 5001, createTime: 1000})

	leftovers, err := Leftovers(dir)
	if err != nil {
		t.Fatalf("Leftovers: %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("leftovers = %#v, want none", leftovers)
	}
	if *calls != 0 {
		t.Fatalf("liveProcesses called %d times, want 0", *calls)
	}
}

// TestLeftoversFindsOnlyTheSecondWorkersGroup asserts every worker
// line is collected, not only the first and the last: the only
// leftover sits in the middle worker's group, and the run is reported
// once, under its own id, with pids ascending in spite of the
// scripted table order. This is the guard the every-worker-line
// collection protects: a parse that kept only the first and last
// worker lines would never see the middle group and this test fails.
func TestLeftoversFindsOnlyTheSecondWorkersGroup(t *testing.T) {
	withLiveProcesses(t, []liveProcess{
		// The second worker's group, scripted out of order so the
		// report must sort.
		{pid: 5021, pgid: 5002, createTime: 2100},
		{pid: 5020, pgid: 5002, createTime: 2500},
		// The third worker's group member is older than the worker
		// and must be excluded.
		{pid: 5030, pgid: 5003, createTime: 1000},
	}, nil)
	dir := t.TempDir()
	path := writeLeftoverRecord(t, dir, "20260830T101500Z-1", 4242, true,
		workerSpec{pid: 5001, createTime: 1000},
		workerSpec{pid: 5002, createTime: 2000},
		workerSpec{pid: 5003, createTime: 3000},
	)

	leftovers, err := Leftovers(dir)
	if err != nil {
		t.Fatalf("Leftovers: %v", err)
	}
	want := []Leftover{{RunID: "20260830T101500Z-1", Path: path, PIDs: []int{5020, 5021}}}
	if !reflect.DeepEqual(leftovers, want) {
		t.Fatalf("leftovers = %#v, want %#v", leftovers, want)
	}
}

// TestLeftoversSkipsWorkerWithUnusableCreateTime asserts a positive
// value from the Unix epoch's sub-second interval is not used as worker
// identity evidence, even when the group contains a current row.
func TestLeftoversSkipsWorkerWithUnusableCreateTime(t *testing.T) {
	withLiveProcesses(t, []liveProcess{
		{pid: 5010, pgid: 5001, createTime: time.Now().UnixMilli()},
	}, nil)
	dir := t.TempDir()
	writeLeftoverRecord(t, dir, "20260830T101500Z-1", 4242, true, workerSpec{pid: 5001, createTime: 1})

	leftovers, err := Leftovers(dir)
	if err != nil {
		t.Fatalf("Leftovers: %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("leftovers = %#v, want none", leftovers)
	}
}

// TestLeftoversSkipsFutureGroupMember asserts a process-table row
// whose creation time is in the future is not reported.
func TestLeftoversSkipsFutureGroupMember(t *testing.T) {
	withLiveProcesses(t, []liveProcess{
		{pid: 5010, pgid: 5001, createTime: time.Now().Add(time.Hour).UnixMilli()},
	}, nil)
	dir := t.TempDir()
	writeLeftoverRecord(t, dir, "20260830T101500Z-1", 4242, true, workerSpec{pid: 5001, createTime: 1000})

	leftovers, err := Leftovers(dir)
	if err != nil {
		t.Fatalf("Leftovers: %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("leftovers = %#v, want none", leftovers)
	}
}

// TestLeftoversExcludesGroupMembersOlderThanWorker asserts the age
// floor: a group member whose creation time is older than the
// worker's already existed before the worker started and was not
// started by it, while a member created at exactly the worker's time
// still counts. This is the guard the age filter protects: without
// it, the older member below would be reported and this test fails.
func TestLeftoversExcludesGroupMembersOlderThanWorker(t *testing.T) {
	withLiveProcesses(t, []liveProcess{
		{pid: 5010, pgid: 5001, createTime: 900},
		{pid: 5011, pgid: 5001, createTime: 1000},
	}, nil)
	dir := t.TempDir()
	path := writeLeftoverRecord(t, dir, "20260830T101500Z-1", 4242, true, workerSpec{pid: 5001, createTime: 1000})

	leftovers, err := Leftovers(dir)
	if err != nil {
		t.Fatalf("Leftovers: %v", err)
	}
	want := []Leftover{{RunID: "20260830T101500Z-1", Path: path, PIDs: []int{5011}}}
	if !reflect.DeepEqual(leftovers, want) {
		t.Fatalf("leftovers = %#v, want %#v", leftovers, want)
	}
}

// TestLeftoversOrdersRunsOldestFirst asserts runs appear in the
// report by run id ascending — oldest first, like the interrupted-run
// reader — because the run id begins with a fixed-width UTC timestamp
// and ReadDir yields names in sorted order.
func TestLeftoversOrdersRunsOldestFirst(t *testing.T) {
	withLiveProcesses(t, []liveProcess{
		{pid: 6010, pgid: 6001, createTime: 2500},
		{pid: 5010, pgid: 5001, createTime: 1500},
	}, nil)
	dir := t.TempDir()
	olderPath := writeLeftoverRecord(t, dir, "20260830T101500Z-1", 4242, true, workerSpec{pid: 5001, createTime: 1000})
	newerPath := writeLeftoverRecord(t, dir, "20260830T102000Z-2", 4243, true, workerSpec{pid: 6001, createTime: 2000})

	leftovers, err := Leftovers(dir)
	if err != nil {
		t.Fatalf("Leftovers: %v", err)
	}
	want := []Leftover{
		{RunID: "20260830T101500Z-1", Path: olderPath, PIDs: []int{5010}},
		{RunID: "20260830T102000Z-2", Path: newerPath, PIDs: []int{6010}},
	}
	if !reflect.DeepEqual(leftovers, want) {
		t.Fatalf("leftovers = %#v, want %#v", leftovers, want)
	}
}

// TestLeftoversNeverSweepsWithoutSettledCandidates asserts the sweep
// stays untouched when no settled record carries a reportable worker:
// a finished record with no worker lines and a record still in flight
// both drop out before the process table is ever consulted.
func TestLeftoversNeverSweepsWithoutSettledCandidates(t *testing.T) {
	calls := withLiveProcesses(t, nil, nil)
	withPidAlive(t, func(pid int32) (bool, error) { return pid == 4242, nil })
	dir := t.TempDir()
	writeLeftoverRecord(t, dir, "20260830T101500Z-1", 4242, true)
	writeLeftoverRecord(t, dir, "20260830T102000Z-2", 4242, false)

	leftovers, err := Leftovers(dir)
	if err != nil {
		t.Fatalf("Leftovers: %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("leftovers = %#v, want none", leftovers)
	}
	if *calls != 0 {
		t.Fatalf("liveProcesses called %d times, want 0", *calls)
	}
}

// TestLeftoversSweepFailureIsSilent asserts a process table that
// cannot be read yields no leftovers and no error: only a records
// directory that cannot be read is an error worth returning, and
// doubtful cases resolve toward silence.
func TestLeftoversSweepFailureIsSilent(t *testing.T) {
	withLiveProcesses(t, nil, errors.New("process table unavailable"))
	dir := t.TempDir()
	writeLeftoverRecord(t, dir, "20260830T101500Z-1", 4242, true, workerSpec{pid: 5001, createTime: 1000})

	leftovers, err := Leftovers(dir)
	if err != nil {
		t.Fatalf("Leftovers = %v, want no error", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("leftovers = %#v, want none", leftovers)
	}
}

// TestLeftoversMissingRecordsDirIsSilent asserts a records directory
// that does not exist produces no leftovers and no error: there are
// no records, hence no runs and nothing to report.
func TestLeftoversMissingRecordsDirIsSilent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	leftovers, err := Leftovers(dir)
	if err != nil || leftovers != nil {
		t.Fatalf("Leftovers(missing dir) = (%#v, %v), want (nil, nil)", leftovers, err)
	}
}

// TestLeftoversIgnoresNonRecordFilesAndWritesNothing asserts only
// *.jsonl files at the top level are records — reported.json, plain
// files, and records inside subdirectories are all ignored — and that
// the records directory is byte-identical afterwards: this reader
// answers the question fresh and never writes a marker of its own.
func TestLeftoversIgnoresNonRecordFilesAndWritesNothing(t *testing.T) {
	withLiveProcesses(t, []liveProcess{
		{pid: 5010, pgid: 5001, createTime: 1500},
		{pid: 6010, pgid: 6001, createTime: 2500},
	}, nil)
	dir := t.TempDir()
	path := writeLeftoverRecord(t, dir, "20260830T101500Z-1", 4242, true, workerSpec{pid: 5001, createTime: 1000})
	// The marker file another reader owns, a non-record file, and a
	// record hidden in a subdirectory: none of them is a record.
	markerPath := filepath.Join(dir, markerFileName)
	if err := os.WriteFile(markerPath, []byte("{\"schemaVersion\":1,\"watermark\":\"20260830T101500Z-1\"}\n"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a record\n"), 0o600); err != nil {
		t.Fatalf("write notes: %v", err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	writeLeftoverRecord(t, sub, "20260830T103000Z-3", 4245, true, workerSpec{pid: 6001, createTime: 2000})
	before := snapshotDir(t, dir)

	leftovers, err := Leftovers(dir)
	if err != nil {
		t.Fatalf("Leftovers: %v", err)
	}
	want := []Leftover{{RunID: "20260830T101500Z-1", Path: path, PIDs: []int{5010}}}
	if !reflect.DeepEqual(leftovers, want) {
		t.Fatalf("leftovers = %#v, want %#v", leftovers, want)
	}
	after := snapshotDir(t, dir)
	if !maps.EqualFunc(before, after, bytes.Equal) {
		t.Fatalf("records directory changed: before %#v, after %#v", before, after)
	}
}

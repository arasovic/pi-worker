package runlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/run"
)

// writeStartRecord writes one hand-built record file into dir: a
// start line carrying the pid and creation time the test chose — a
// zero creation time writes the line without the createTime key, the
// exact shape of every record written before the field existed — the
// given worker lines in order, and, when finished is true, a finish
// line. Pids and creation times are chosen by the test, never read
// back out of a record, so the scripted seams' answers stay
// independent of what the record carries.
func writeStartRecord(t *testing.T, dir, runID string, pid int, startCreateTime int64, finished bool, workers ...workerSpec) string {
	t.Helper()
	lines := []map[string]any{
		{
			"schemaVersion": schemaVersion,
			"event":         "start",
			"runId":         runID,
			"startedAt":     "2026-08-30T10:15:00Z",
			"workspace":     "/workspace",
			"pid":           pid,
			"tasks":         []any{},
		},
	}
	if startCreateTime != 0 {
		lines[0]["createTime"] = startCreateTime
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

// withPidCreateTime replaces the creation-time seam for the duration
// of one test, scripted like the other seams in this repo.
func withPidCreateTime(t *testing.T, createTime func(int) (int64, error)) {
	t.Helper()
	original := pidCreateTime
	pidCreateTime = createTime
	t.Cleanup(func() { pidCreateTime = original })
}

// TestStartLineCarriesCreateTime asserts the start line carries the
// process creation time the pidCreateTime seam returned, as the raw
// millisecond number — the exact-equality identity, never a formatted
// time. The expected value is a literal written here, never read back
// from the code under test, and its sub-second digits prove no
// precision was lost on the way to the line.
func TestStartLineCarriesCreateTime(t *testing.T) {
	const wantCreateTime = int64(1724998530123)
	withPidCreateTime(t, func(pid int) (int64, error) { return wantCreateTime, nil })

	dir := t.TempDir()
	if _, err := Start(dir, time.Now(), "/workspace", []run.Task{{Prompt: "p", Model: "acme/m-1"}}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	lines := recordLines(t, readRecord(t, dir))
	start := decodeLine(t, lines[0])
	if start["event"] != "start" {
		t.Fatalf("line event = %v, want start", start["event"])
	}
	if start["createTime"] != float64(wantCreateTime) {
		t.Fatalf("createTime = %v, want %d", start["createTime"], wantCreateTime)
	}
}

// TestStartLineOmitsCreateTimeOnLookupError asserts a failed
// pidCreateTime lookup leaves the createTime key off the start line
// entirely, never as a zero: the raw bytes of the line do not contain
// it. The absent field is the real, permanent shape of every record
// written before this field existed, so a later reader must be able to
// see "no field".
func TestStartLineOmitsCreateTimeOnLookupError(t *testing.T) {
	withPidCreateTime(t, func(pid int) (int64, error) { return 0, errors.New("no process table entry") })

	dir := t.TempDir()
	recorder, err := Start(dir, time.Date(2026, 8, 30, 4, 15, 30, 0, time.UTC), "/workspace", []run.Task{{Prompt: "p", Model: "acme/m-1"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer recorder.file.Close()

	lines := recordLines(t, readRecord(t, dir))
	if len(lines) != 1 {
		t.Fatalf("record lines = %d, want 1", len(lines))
	}
	if bytes.Contains([]byte(lines[0]), []byte("createTime")) {
		t.Fatalf("start line carries createTime after a failed lookup: %s", lines[0])
	}
	if decodeLine(t, lines[0])["event"] != "start" {
		t.Fatalf("line event = %v, want start", decodeLine(t, lines[0])["event"])
	}
}

// TestReusedStartPidWithDifferentCreateTimeClassifiesInterrupted is
// the fix of this brief, asserted through a real reader — List, which
// must give the record outcome "interrupted", the same classification
// the interrupted-run scan makes. The start pid is alive — the
// number was given to an unrelated long-lived process — but that
// process's creation time differs from the one the record carries, so
// the pair says the writer is gone. Were the creation time ignored,
// the alive pid alone would classify the run as still running.
func TestReusedStartPidWithDifferentCreateTimeClassifiesInterrupted(t *testing.T) {
	withPidAlive(t, func(pid int32) (bool, error) { return pid == 4242, nil })
	// The live process holding pid 4242 was created at 9999, not at
	// the 1000 the record carries.
	withPidCreateTime(t, func(pid int) (int64, error) { return 9999, nil })

	dir := t.TempDir()
	path := writeStartRecord(t, dir, "20260830T101500Z-1", 4242, 1000, false)

	runs, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []Run{{RunID: "20260830T101500Z-1", StartedAt: "2026-08-30T10:15:00Z", Workspace: "/workspace", Outcome: "interrupted", Path: path}}
	if !slices.Equal(runs, want) {
		t.Fatalf("runs = %#v, want %#v", runs, want)
	}
}

// TestRecordWithoutCreateTimeStillClassifiesByPidAlone is the floor:
// a record with no creation time whose pid is alive is still alive,
// exactly as such a record behaved before this field existed. Old
// records on disk must keep behaving as they do today, and the
// creation-time seam must not even be consulted for them — the absent
// field is not a zero to compare, it is no claim at all.
func TestRecordWithoutCreateTimeStillClassifiesByPidAlone(t *testing.T) {
	withPidAlive(t, func(pid int32) (bool, error) { return pid == 4242, nil })
	withPidCreateTime(t, func(pid int) (int64, error) {
		t.Fatalf("pidCreateTime consulted for a record without createTime")
		return 0, nil
	})

	dir := t.TempDir()
	path := writeStartRecord(t, dir, "20260830T101500Z-1", 4242, 0, false)

	runs, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []Run{{RunID: "20260830T101500Z-1", StartedAt: "2026-08-30T10:15:00Z", Workspace: "/workspace", Outcome: "running", Path: path}}
	if !slices.Equal(runs, want) {
		t.Fatalf("runs = %#v, want %#v", runs, want)
	}
}

// TestCreationTimeLookupErrorMeansAlive asserts a record whose start
// pid is alive and whose creation time cannot be looked up counts as
// alive: the identity pair cannot be completed, so the pid alone must
// not be contradicted. Doubtful cases resolve toward silence — here
// the run still reads as running — never toward a wrong accusation
// of interruption.
func TestCreationTimeLookupErrorMeansAlive(t *testing.T) {
	withPidAlive(t, func(pid int32) (bool, error) { return pid == 4242, nil })
	withPidCreateTime(t, func(pid int) (int64, error) { return 0, errors.New("no process table entry") })

	dir := t.TempDir()
	path := writeStartRecord(t, dir, "20260830T101500Z-1", 4242, 1000, false)

	runs, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []Run{{RunID: "20260830T101500Z-1", StartedAt: "2026-08-30T10:15:00Z", Workspace: "/workspace", Outcome: "running", Path: path}}
	if !slices.Equal(runs, want) {
		t.Fatalf("runs = %#v, want %#v", runs, want)
	}
}

// TestLeftoversReportsRunWhoseStartPidWasReused asserts the leftover
// reader now reports the processes of a run it previously hid forever:
// the writer's pid was given to an unrelated long-lived process, so
// the old number-alone liveness question answered alive, the run
// never settled, and its survivor was never reported. The record
// carries the writer's creation time, the live holder of the number
// was created at a different time, so the pair says the writer is
// gone — the run is settled, and the survivor in the recorded
// worker's group is reported. A live process sits in that group in
// the scripted table, which is what makes the report observable.
func TestLeftoversReportsRunWhoseStartPidWasReused(t *testing.T) {
	withLiveProcesses(t, []liveProcess{
		// The worker's old group still holds a survivor.
		{pid: 5010, pgid: 5001, createTime: 1100},
	}, nil)
	withPidAlive(t, func(pid int32) (bool, error) { return pid == 4242, nil })
	// The live process holding start pid 4242 was created at 9999,
	// not at the 1000 the record carries.
	withPidCreateTime(t, func(pid int) (int64, error) { return 9999, nil })

	dir := t.TempDir()
	path := writeStartRecord(t, dir, "20260830T101500Z-1", 4242, 1000, false, workerSpec{pid: 5001, createTime: 1000})

	leftovers, err := Leftovers(dir)
	if err != nil {
		t.Fatalf("Leftovers: %v", err)
	}
	want := []Leftover{{RunID: "20260830T101500Z-1", Path: path, PIDs: []int{5010}}}
	if !reflect.DeepEqual(leftovers, want) {
		t.Fatalf("leftovers = %#v, want %#v", leftovers, want)
	}
}

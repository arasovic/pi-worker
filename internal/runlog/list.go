package runlog

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/arasovic/pi-worker/internal/run"
)

// Run is the listed shape of one run record: the start line's display
// fields, the outcome, and the record's path. It is an inventory
// entry, not the record itself.
type Run struct {
	RunID     string `json:"runId"`
	StartedAt string `json:"startedAt"`
	Workspace string `json:"workspace"`
	Tasks     int    `json:"tasks"`
	Outcome   string `json:"outcome"`
	Path      string `json:"path"`
}

// List returns one entry per run record in dir, newest first — run ids
// begin with a fixed-width UTC timestamp, so sorting the ids as plain
// strings descending is chronological; no time is parsed to sort.
//
// The outcome is decided by the same facts and the same liveness seam
// Interrupted uses, never a second answer to the same question:
//
//   - the run's own outcome, verbatim, when the finish line carries a
//     result;
//   - "error" when the finish line carries the error arm instead — the
//     writer guarantees exactly one of the two;
//   - "running" when there is no finish line and the start line's
//     process — its pid paired with the line's creation time, when
//     one is recorded — is still alive, a doubtful lookup counting
//     as alive, the same fail-safe direction as the interrupted-run
//     warning;
//   - "interrupted" when there is no finish line and that process is
//     no longer alive;
//   - "unknown" when the record has no start line that parses — the
//     record is still listed, with empty display fields, because this
//     is an inventory and hiding an entry it cannot read would be
//     worse than showing it with what the file name tells.
//
// A missing records directory is not an error: there are no records to
// list. Only *.jsonl files are records: reported.json, its .tmp-*
// stages, and any other file are skipped silently, exactly as the
// interrupted-run scan skips them. List never writes anything: it does
// not touch reported.json, does not advance any watermark, and prints
// no interrupted-run warning.
func List(dir string) ([]Run, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		// A missing records directory is not an error: there are no
		// records, hence no runs to list.
		if os.IsNotExist(err) {
			return []Run{}, nil
		}
		return nil, err
	}
	runs := make([]Run, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		// Only *.jsonl files are records; the marker file itself is
		// excluded by the same filter and must stay excluded.
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		path := filepath.Join(dir, name)
		runID := strings.TrimSuffix(name, ".jsonl")
		rec, err := parseRecord(path)
		if err != nil {
			// An unknown record is still listed, carrying what the file
			// name alone tells: the run id and the path.
			runs = append(runs, Run{RunID: runID, Outcome: "unknown", Path: path})
			continue
		}
		run := Run{
			RunID:     runID,
			StartedAt: rec.startedAt,
			Workspace: rec.workspace,
			Tasks:     rec.tasks,
			Path:      path,
		}
		switch {
		case rec.finished && rec.hasResult:
			// The finish line carries the run's own result: its outcome
			// is copied verbatim, never mapped or renamed.
			run.Outcome = rec.resultOutcome
		case rec.finished:
			// A finish line without a result is the error arm: the run
			// returned an error instead of a result, the two being
			// mutually exclusive in the record format.
			run.Outcome = "error"
		default:
			// No finish line: the run is either still in progress or was
			// interrupted, decided by the same liveness question the
			// interrupted-run scan and the leftover reader ask, answered
			// by the shared recordProcessAlive helper. A doubtful case
			// counts as alive — a liveness error, a creation-time lookup
			// error, a record without a creation time — so doubtful
			// cases resolve toward a live run, never toward a wrong
			// accusation of interruption.
			if recordProcessAlive(rec.pid, rec.createTime) {
				run.Outcome = "running"
			} else {
				run.Outcome = "interrupted"
			}
		}
		runs = append(runs, run)
	}
	// Newest first: the fixed-width timestamp at the start of every run
	// id makes plain string order chronological.
	sort.Slice(runs, func(i, j int) bool { return runs[i].RunID > runs[j].RunID })
	return runs, nil
}

// recordFacts is the shared parse of one record file, answering
// everything the three readers ask of it: the process identity and
// the display fields from the start line, the completion facts from
// the finish line, and every worker line's process identity. List
// reads all of it; InspectRecord needs only the pid and the finished
// flag, and the readers must agree on those facts — the interrupted-
// run warning, the list, and the leftover report classify the same
// record the same way.
type recordFacts struct {
	// pid is the process that wrote the record, from the start line.
	pid int
	// createTime is that process's creation time from the same line,
	// the second half of the pid's identity. Zero means the line
	// carried no creation time: a lookup that failed at write time or
	// a record written before the field existed. Only a positive
	// value is ever compared — a non-positive one, absent, zero, or
	// negative, is no real creation time and carries no identity to
	// compare.
	createTime int64
	// workers holds every worker line's process identity, in record
	// order: the pid each worker launched paired with its creation
	// time. A record can carry up to three worker lines, one per
	// worker goroutine, and the leftover reader needs each group
	// under the run — a leftover in the second worker's group must
	// still be found.
	workers []workerFacts
	// startedAt, workspace, and tasks are the start line's display
	// fields, copied verbatim.
	startedAt string
	workspace string
	tasks     int
	// finished reports whether the record carries its finish line: its
	// last non-empty line decodes with event "finish".
	finished bool
	// hasResult reports whether the finish line carries a result; when
	// it does, resultOutcome is the result's outcome verbatim.
	hasResult     bool
	resultOutcome string
}

// maxRecordBytes is the ceiling on one record file's size, in bytes.
// A real record is a handful of lines of JSON — kilobytes — so 32 MiB
// is four orders of magnitude of headroom and still cannot exhaust
// memory. readRecordFile refuses a record above the ceiling before it
// is read.
const maxRecordBytes int64 = 32 << 20

// readRecordFile reads one record file, refusing anything that is not
// a regular file and anything above maxRecordBytes before it opens or
// reads the path. The regular-file check comes from os.Lstat, never
// os.Stat: Lstat does not follow a symlink, so a symlink reports mode
// ModeSymlink and is refused outright, while Stat would follow it to
// its target and let the escape through. A FIFO with no writer blocks
// in the open — not the read — so the refusal must happen before any
// open, or one planted record name hangs the whole product.
//
// After the open, the file that was opened is checked itself — the
// pattern of readStableRegularFile in internal/skillinstall: it must
// still be a regular file, it must be the very file the checks
// described, and it must still be within the size ceiling. A name
// replaced between the check and the open is thereby refused rather
// than read: nothing ties the file that was checked to the file that
// the open returns, so without the re-check the checks describe one
// file and the read opens another. A record that stays the same file
// but grows under the reader needs no further guard — a torn last
// line is explicitly expected and documented in parseRecord.
//
// One ceiling is accepted by design, and its failure direction is a
// stall, never a wrong answer: the checks refuse a name that was
// already a named pipe before them, but a name that becomes one
// between the check and the open blocks the open itself, and an open
// that blocks cannot be interrupted from here. The cost is this
// reader hanging on that one planted name — nothing is ever read
// from the pipe, and no classification is made: the reader simply
// does not return.
func readRecordFile(path string) (data []byte, err error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("record is not a regular file")
	}
	if before.Size() < 0 || before.Size() > maxRecordBytes {
		return nil, errors.New("record is too large")
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
			if err != nil {
				data = nil
			}
		}
	}()

	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	// The re-check of the open file: a name replaced between the
	// check and the open hands the open a different file, and a file
	// that grew past the ceiling after the check must not be read
	// whole. The first condition also refuses a symlink that
	// appeared in the gap.
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, errors.New("record changed before reading")
	}
	if opened.Size() < 0 || opened.Size() > maxRecordBytes {
		return nil, errors.New("record is too large")
	}

	data, err = io.ReadAll(io.LimitReader(f, maxRecordBytes))
	if err != nil {
		return nil, err
	}
	return data, nil
}

// parseRecord reads one record and answers everything the three
// readers ask of it. It is the shared parse underneath inspectRecord
// and Leftovers: List needs more than the interrupted-run scan does,
// and duplicating the read and the line walk would give the readers a
// second chance to disagree about one record. A record that cannot be
// read or parsed returns an error, which all callers treat as settled;
// that includes a start line that is not a start line or carries no
// usable pid.
//
// The classification facts — the start line's event, pid and
// creation time, the last line's finish event — are decoded exactly
// as the interrupted-run scan always decoded them, so the scan's
// answers cannot move. The display fields (started-at, workspace, task count) and the finish
// line's result are decoded separately and best-effort: a malformed
// display field must not change what the scan reports, and a record
// being written right now can end in a partial last line, which never
// makes a live run look interrupted — the start line decides the
// classification whenever it parses.
func parseRecord(path string) (recordFacts, error) {
	data, err := readRecordFile(path)
	if err != nil {
		return recordFacts{}, err
	}
	lines := strings.Split(string(data), "\n")
	first, last := -1, -1
	var workers []workerFacts
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if first == -1 {
			first = i
		}
		last = i
		// Every non-empty line is a potential worker line, collected
		// independently of the classification facts: a record can
		// carry up to three of them, and a worker line between the
		// first and the last must still be collected, or a leftover
		// in its group would be invisible.
		var worker workerLine
		if err := json.Unmarshal([]byte(line), &worker); err == nil && worker.Event == "worker" && worker.PID > 0 {
			workers = append(workers, workerFacts{pid: worker.PID, createTime: worker.CreateTime})
		}
	}
	if first == -1 {
		return recordFacts{}, errors.New("record has no lines")
	}
	var start struct {
		Event      string `json:"event"`
		PID        int    `json:"pid"`
		CreateTime int64  `json:"createTime"`
	}
	if err := json.Unmarshal([]byte(lines[first]), &start); err != nil || start.Event != "start" || start.PID <= 0 {
		return recordFacts{}, errors.New("record has no usable start line")
	}
	rec := recordFacts{pid: start.PID, createTime: start.CreateTime, workers: workers}
	// The display fields are best-effort: a malformed one leaves them
	// zero without touching the classification, which is decided by the
	// minimal facts above.
	var display struct {
		StartedAt string            `json:"startedAt"`
		Workspace string            `json:"workspace"`
		Tasks     []json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(lines[first]), &display); err == nil {
		rec.startedAt = display.StartedAt
		rec.workspace = display.Workspace
		rec.tasks = len(display.Tasks)
	}
	var finish struct {
		Event string `json:"event"`
	}
	if err := json.Unmarshal([]byte(lines[last]), &finish); err != nil || finish.Event != "finish" {
		// No finish line — including a torn last line that does not
		// decode — so the run was still in flight when it stopped
		// writing; the liveness seam decides running from interrupted.
		return rec, nil
	}
	rec.finished = true
	var lastLine struct {
		Result *run.Result `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[last]), &lastLine); err == nil && lastLine.Result != nil {
		rec.hasResult = true
		rec.resultOutcome = string(lastLine.Result.Outcome)
	}
	return rec, nil
}

// recordProcessAlive answers the one liveness question the three
// readers ask of a record's start-line process: is that process still
// the process that wrote the record? The answer is decided by the
// pid and, when the record carries a positive one, the creation time
// paired with it — the pair is the identity, and an unrelated process
// holding a reused number carries a different creation time. Every
// doubtful case resolves toward alive, the same fail-safe direction
// as every reader here: silence, never a wrong accusation of a dead
// run.
//
// The decision, row for row:
//
//   - pidAlive errors: alive.
//   - pidAlive reports not alive: dead.
//   - the pid does not fit the range the lookup takes — not
//     positive, or beyond its int32 range: alive, the fail-safe
//     answer exactly like an errored lookup, and never truncated
//     into the lookup's range, where it would name a different
//     process. The writer only ever records its own pid, which
//     always fits, so this row guards a record that was corrupted
//     or edited by hand, not one the product wrote.
//   - pidAlive reports alive, the record carries no positive
//     creation time — absent, zero, or negative: alive — the number
//     alone decides, so a record written before the field existed
//     behaves exactly as it always did, and a negative number,
//     never a real creation time, is treated exactly like an absent
//     one.
//   - pidAlive reports alive, the record carries a positive
//     creation time, and the creation-time lookup errors: alive.
//   - pidAlive reports alive, the record carries a positive
//     creation time, and the lookup reports a non-positive
//     creation time: alive — a lookup that succeeded but reports a
//     value no real process can have proves nothing, so it resolves
//     toward alive rather than toward a mismatch.
//   - pidAlive reports alive, the record carries a positive
//     creation time, and the lookup reports a different one: dead —
//     the number was reused by an unrelated process.
//   - pidAlive reports alive, the record carries a positive
//     creation time, and the lookup reports the same one: alive.
func recordProcessAlive(pid int, createTime int64) bool {
	if pid <= 0 || pid > math.MaxInt32 {
		// A pid outside the range the liveness lookup takes — or not
		// positive at all — is not a pid this reader can answer for:
		// truncating it into int32 would name a different process.
		// The fail-safe answer is alive, exactly like an errored
		// lookup. The writer only ever records its own pid, which
		// always fits this range, so this guard is for a record that
		// was corrupted or edited by hand, not one the product
		// wrote.
		return true
	}
	alive, err := pidAlive(int32(pid))
	if err != nil {
		return true
	}
	if !alive {
		return false
	}
	if createTime <= 0 {
		// A non-positive creation time is no claim at all: absent,
		// zero, or negative, no real process can have been created
		// at it, so the pid alone decides — a live number reads as a
		// live process.
		return true
	}
	seen, err := pidCreateTime(pid)
	if err != nil || seen <= 0 {
		return true
	}
	return seen == createTime
}

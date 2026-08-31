package runlog

import (
	"encoding/json"
	"errors"
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
//     process is still alive, a liveness error counting as alive, the
//     same fail-safe direction as the interrupted-run warning;
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
			// interrupted, decided by the same two-armed liveness test
			// the interrupted-run scan makes, through the same seam. A
			// liveness error counts as alive — doubtful cases resolve
			// toward a live run, never toward a wrong accusation of
			// interruption.
			alive, err := pidAlive(int32(rec.pid))
			if err != nil || alive {
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
// everything both readers ask of it: the process identity and the
// display fields from the start line, and the completion facts from
// the finish line. List reads all of it; InspectRecord needs only the
// pid and the finished flag, and the two readers must agree on those
// two facts — the interrupted-run warning and the list classify the
// same record the same way.
type recordFacts struct {
	// pid is the process that wrote the record, from the start line.
	pid int
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

// parseRecord reads one record and answers everything both readers ask
// of it. It is the shared parse underneath inspectRecord: List needs
// more than the interrupted-run scan does, and duplicating the read
// and the line walk would give the two readers a second chance to
// disagree. A record that cannot be read or parsed returns an error,
// which both callers treat as settled; that includes a start line that
// is not a start line or carries no usable pid.
//
// The classification facts — the start line's event and pid, the last
// line's finish event — are decoded exactly as the interrupted-run
// scan always decoded them, so the scan's answers cannot move. The
// display fields (started-at, workspace, task count) and the finish
// line's result are decoded separately and best-effort: a malformed
// display field must not change what the scan reports, and a record
// being written right now can end in a partial last line, which never
// makes a live run look interrupted — the start line decides the
// classification whenever it parses.
func parseRecord(path string) (recordFacts, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return recordFacts{}, err
	}
	lines := strings.Split(string(data), "\n")
	first, last := -1, -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if first == -1 {
			first = i
		}
		last = i
	}
	if first == -1 {
		return recordFacts{}, errors.New("record has no lines")
	}
	var start struct {
		Event string `json:"event"`
		PID   int    `json:"pid"`
	}
	if err := json.Unmarshal([]byte(lines[first]), &start); err != nil || start.Event != "start" || start.PID <= 0 {
		return recordFacts{}, errors.New("record has no usable start line")
	}
	rec := recordFacts{pid: start.PID}
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

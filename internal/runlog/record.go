// Package runlog writes the on-disk record of one run while the run is
// in flight. pi-worker can be killed without warning — the caller hits
// Ctrl-C, the run hits its timeout, the terminal closes, or the
// supervising process is killed outright — and on some of those paths no
// code of ours runs afterwards, so no signal handler can save the run.
// The only design that survives that is a record written as the run
// progresses and left on disk: the start line is written before the run
// starts, one worker line per started worker while the run is in
// flight, the finish line after the run returns, and a record whose
// finish line never arrived is how a later reader learns the run was
// interrupted.
//
// This package both writes and reads records. The writer stores one
// record per run while the run is in flight; Interrupted, the reader,
// scans a records directory for earlier runs that were interrupted — a
// record with no finish line whose process is no longer alive — and
// the CLI warns about them once. Ctrl-C is deliberately not an
// interruption by that definition: a Ctrl-C'd run writes its finish
// line with outcome cancelled, so only a no-grace kill leaves a record
// without one.
package runlog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/arasovic/pi-worker/internal/config"
	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/run"
	"github.com/shirou/gopsutil/v4/process"
)

// schemaVersion is the run-record document version. It is the record's
// own version, independent of the run result's schemaVersion: the two
// documents evolve separately.
const schemaVersion = 1

// promptCap is the largest prompt the start line records verbatim, in
// bytes. A longer prompt is cut to this many bytes and marked truncated
// in the record; the run itself is unaffected — the worker always
// receives the full prompt. The cap bounds the record, not the run.
const promptCap = 4096

// pidCreateTime is the private dependency-injection seam behind the
// start line's and worker line's createTime fields: the creation
// time of the process that wrote the record — the pi-worker itself
// for the start line, the process a worker started for a worker line
// — in milliseconds since the Unix epoch exactly as gopsutil reports
// it. Tests replace it with a scripted answer so the records they
// write stay hermetic; production never does. The value
// is an exact-equality identity check, never a formatted time: a
// string round-trip would lose sub-second precision and the equality
// against process.CreateTime would silently stop matching.
var pidCreateTime = defaultPidCreateTime

// defaultPidCreateTime returns the creation time of the process with
// the given pid, in milliseconds since the Unix epoch. A process that
// cannot be looked up returns an error; the caller leaves the field
// off the record instead of inventing a value.
func defaultPidCreateTime(pid int) (int64, error) {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return 0, err
	}
	return p.CreateTime()
}

// Dir returns the directory run records live in. Records are never put
// inside a workspace: a workspace is caller-owned and may be ephemeral
// or shared, while a run record must outlive every path by which the
// run can die.
func Dir() (string, error) {
	userDir, err := config.UserDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(userDir, "runs"), nil
}

// Recorder writes the three kinds of lines of one run's record: the
// start line at Start, one worker line per started worker while the run
// is in flight, and the finish line at Finish. A Recorder is shared
// between the goroutine that called Start and will call Finish and up
// to three worker goroutines calling WorkerProcess concurrently. The
// mutex guards the shared write-error slot and the ordering between a
// worker line and the final write-and-close, exactly as the field
// comment below states; appending to a file opened in append mode
// already keeps each line whole, so no write needs guarding for its
// own sake.
type Recorder struct {
	file  *os.File
	runID string

	// mu guards the shared write-error slot and the ordering between a
	// worker line and the final write-and-close: appending to a file
	// opened in append mode already keeps each line whole, so no write
	// needs guarding for its own sake.
	mu sync.Mutex
	// writeErr is the first write error any worker line saw. It is kept
	// rather than warned about inline because a warning printed from
	// three concurrent workers would interleave with the run's own
	// output; Finish returns it once its own write succeeded, so a
	// dropped worker line still surfaces once, through the warning the
	// CLI already prints.
	writeErr error
}

// RunID returns the shared identity of a run record before the record
// exists: startedAt in UTC as 20060102T150405Z, a hyphen, and the
// writer's process id. Start uses it for the record file's name and
// every line's runId.
func RunID(startedAt time.Time) string {
	// The id embeds the process id so two runs starting in the same
	// second cannot collide: the timestamp's finest unit is the second,
	// and the process id separates the runs that share it.
	return startedAt.UTC().Format("20060102T150405Z") + "-" + strconv.Itoa(os.Getpid())
}

// Start writes the start line of one run's record before the run
// begins: the run's identity, workspace, and each task's projection —
// model, thinking level, the prompt (capped), the write declaration,
// and for every file carried into the prompt its path, byte count and
// SHA-256, never its content. The record's directory is created if
// missing, the record file is opened for append, and the complete line
// including its newline reaches the file in one Write call before Start
// returns, so a run killed at any later instant still leaves its start
// line on disk. The creation-time lookup and the marshalling run
// before the file is opened: everything that can fail or block is done
// first, so the only moment the file exists without its start line is
// the single Write that follows the open.
//
// The record carries the identity of the process each worker starts:
// one worker line per started worker, appended by WorkerProcess while
// the run is in flight — the only moment that identity exists and can
// be recorded. The start line carries the writer's own process
// identity, the same pid paired with its creation time, so a later
// reader can still tell the run from a number that was reused by an
// unrelated process. Records are never deleted, not on success and
// not on failure.
func Start(dir string, startedAt time.Time, workspace string, tasks []run.Task) (*Recorder, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	runID := RunID(startedAt)
	// The creation-time lookup and the marshalling of the start line
	// happen before the record file exists: everything that can fail
	// or block — the process-table read, the JSON encoding — completes
	// before the open, so a marshal failure leaves no file behind, and
	// the file exists without its start line only for the length of
	// the single Write that follows the open. A concurrent scan that
	// catches it in that window sees a zero-length record, which the
	// interrupted-run reader treats as a record not yet written —
	// never as a corrupt one — and examines again on its next scan. A
	// lookup failure leaves the createTime key off the start line,
	// exactly as it leaves it off a worker line — the identity is
	// weaker then, never an error. The process is this worker itself,
	// the same process whose pid the line records.
	createTime, err := pidCreateTime(os.Getpid())
	if err != nil {
		createTime = 0
	}
	line, err := json.Marshal(startLine{
		SchemaVersion: schemaVersion,
		Event:         "start",
		RunID:         runID,
		StartedAt:     startedAt.UTC().Format(time.RFC3339),
		Workspace:     workspace,
		PID:           os.Getpid(),
		Tasks:         projectTasks(tasks),
		CreateTime:    createTime,
	})
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, runID+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		file.Close()
		return nil, err
	}
	return &Recorder{file: file, runID: runID}, nil
}

// WorkerProcess appends the worker line of the run's record: the
// identity of the process one worker started, written while the run is
// in flight — the only moment that identity exists and can be recorded.
// It appends one line and returns nothing; a write failure is kept as
// the recorder's first write error and surfaced once by Finish, never
// printed from the concurrent worker goroutine. WorkerProcess on a nil
// Recorder is a no-op, like Finish.
func (r *Recorder) WorkerProcess(at time.Time, workerID int, pid int) {
	if r == nil {
		return
	}
	// The creation-time lookup happens before the lock: reading the
	// process table takes real time, and the lock only guards the
	// shared write-error slot and the ordering between a worker line
	// and the final write-and-close. A lookup failure leaves the
	// createTime key off the line — the identity is weaker then, never
	// an error.
	//
	// One limit is accepted by design: the creation time is sampled
	// here, after the child has started, so a child that exits before
	// this lookup runs can be reaped and its pid number reused by an
	// unrelated process, and the line would then record that process's
	// creation time as the worker's identity. The window is the time
	// from the child's start to this lookup — milliseconds, against
	// the minutes a pid number takes to cycle, and within it the child
	// must exit, be reaped, and have its number taken — and the
	// failure costs a wrong group in one warning line, never a signal.
	createTime, err := pidCreateTime(pid)
	if err != nil {
		createTime = 0
	}
	line := workerLine{
		SchemaVersion: schemaVersion,
		Event:         "worker",
		RunID:         r.runID,
		At:            at.UTC().Format(time.RFC3339),
		WorkerID:      workerID,
		PID:           pid,
		CreateTime:    createTime,
	}
	data, err := json.Marshal(line)
	if err != nil {
		r.mu.Lock()
		if r.writeErr == nil {
			r.writeErr = err
		}
		r.mu.Unlock()
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.file.Write(append(data, '\n')); err != nil && r.writeErr == nil {
		r.writeErr = err
	}
}

// Finish appends the finish line of the run's record, the only
// completion marker: the run's result marshalled exactly as the run
// already marshals it, or the run-level error text when the run
// returned an error instead of a result. Exactly one of the two is
// present. The complete line including its newline is one Write call.
// When the finish line itself wrote and closed cleanly but a worker
// line written earlier failed, Finish returns that first error, so the
// dropped worker line still surfaces through the caller's existing
// warning. Finish on a nil Recorder is a no-op returning nil, so
// callers never branch on whether the record could be started.
func (r *Recorder) Finish(finishedAt time.Time, result *run.Result, runErr error) error {
	if r == nil {
		return nil
	}
	line := finishLine{
		SchemaVersion: schemaVersion,
		Event:         "finish",
		RunID:         r.runID,
		FinishedAt:    finishedAt.UTC().Format(time.RFC3339),
	}
	if runErr != nil {
		line.Error = runErr.Error()
	} else {
		line.Result = result
	}
	data, err := json.Marshal(line)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.file.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := r.file.Close(); err != nil {
		return err
	}
	if r.writeErr != nil {
		return r.writeErr
	}
	return nil
}

// projectTasks projects the run's tasks into the start line. This is
// the point where content must not leak: run.Task carries the bytes of
// the files composed into the prompt, and only their path, byte count
// and SHA-256 are recorded. The hash is computed over the content
// exactly as carried, so it matches a checksum of the file on disk and
// stays consistent with the recorded byte count. The write declaration
// keeps its two facts separate, exactly as in run.WriteDeclaration: a
// task that declared nothing differs from a task that declared an empty
// write set, and collapsing them would erase that difference.
func projectTasks(tasks []run.Task) []startTask {
	projected := make([]startTask, len(tasks))
	for i, task := range tasks {
		prompt, truncated := capPrompt(task.Prompt)
		data := make([]pi.DataFile, 0, len(task.Data))
		for _, file := range task.Data {
			sum := sha256.Sum256(file.Content)
			data = append(data, pi.DataFile{Path: file.Path, Bytes: len(file.Content), SHA256: hex.EncodeToString(sum[:])})
		}
		projected[i] = startTask{
			Model:           task.Model,
			ThinkingLevel:   string(task.ThinkingLevel),
			Prompt:          prompt,
			PromptTruncated: truncated,
			WritesDeclared:  task.Writes.Declared,
			Writes:          task.Writes.Paths,
			Data:            data,
		}
	}
	return projected
}

// capPrompt bounds a recorded prompt to promptCap bytes. A prompt at or
// below the cap is returned verbatim with truncated false. A longer
// prompt is cut to the cap and then loses trailing bytes while the
// result is not valid UTF-8, so a multi-byte character is never split:
// the cut backs off to the last complete character boundary at or
// before the cap.
func capPrompt(prompt string) (string, bool) {
	if len(prompt) <= promptCap {
		return prompt, false
	}
	cut := prompt[:promptCap]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut, true
}

// startLine is the first line of a run record, written before the run
// starts.
type startLine struct {
	SchemaVersion int         `json:"schemaVersion"`
	Event         string      `json:"event"`
	RunID         string      `json:"runId"`
	StartedAt     string      `json:"startedAt"`
	Workspace     string      `json:"workspace"`
	PID           int         `json:"pid"`
	Tasks         []startTask `json:"tasks"`
	// CreateTime is the process creation time of the process that
	// wrote the start line — the pi-worker itself — in milliseconds
	// since the Unix epoch, exactly as gopsutil reports it, for exact
	// equality against a later Process.CreateTime. Absent when the
	// lookup failed at write time, which is also the shape of every
	// record written before this field existed.
	CreateTime int64 `json:"createTime,omitempty"`
}

// startTask is one task's projection in the start line. WritesDeclared
// is always present; Writes carries the declared paths when there are
// any, and Data carries the carried-file reports when the task carried
// material.
type startTask struct {
	Model           string        `json:"model"`
	ThinkingLevel   string        `json:"thinkingLevel"`
	Prompt          string        `json:"prompt"`
	PromptTruncated bool          `json:"promptTruncated"`
	WritesDeclared  bool          `json:"writesDeclared"`
	Writes          []string      `json:"writes,omitempty"`
	Data            []pi.DataFile `json:"data,omitempty"`
}

// workerLine is the line of a run record written while the run is in
// flight, one per started worker, carrying the identity of the process
// that worker launched: the pid paired with the process's creation
// time, the pair being the identity — a pid alone is reused, so it
// cannot name a process on its own.
type workerLine struct {
	SchemaVersion int    `json:"schemaVersion"`
	Event         string `json:"event"`
	RunID         string `json:"runId"`
	At            string `json:"at"`
	WorkerID      int    `json:"workerId"`
	PID           int    `json:"pid"`
	// CreateTime is the process creation time in milliseconds since
	// the Unix epoch, exactly as gopsutil reports it, for exact
	// equality against a later Process.CreateTime. Absent when the
	// lookup failed at write time, which is also the shape of every
	// record written before this field existed.
	CreateTime int64 `json:"createTime,omitempty"`
}

// finishLine is the final line of a run record. Result and Error are
// mutually exclusive: the run returned exactly one of them.
type finishLine struct {
	SchemaVersion int         `json:"schemaVersion"`
	Event         string      `json:"event"`
	RunID         string      `json:"runId"`
	FinishedAt    string      `json:"finishedAt"`
	Result        *run.Result `json:"result,omitempty"`
	Error         string      `json:"error,omitempty"`
}

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
// interrupted. Nothing in this package reads records; it only writes
// them, one record per run.
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
// to three worker goroutines calling WorkerProcess concurrently, so
// every write is guarded by a mutex.
type Recorder struct {
	file  *os.File
	runID string

	// mu guards every file write and the write-error slot below:
	// WorkerProcess runs concurrently from up to three worker
	// goroutines while the run is in flight, and Finish runs after the
	// run returns.
	mu sync.Mutex
	// writeErr is the first write error any worker line saw. It is kept
	// rather than warned about inline because a warning printed from
	// three concurrent workers would interleave with the run's own
	// output; Finish returns it once its own write succeeded, so a
	// dropped worker line still surfaces once, through the warning the
	// CLI already prints.
	writeErr error
}

// Start writes the start line of one run's record before the run
// begins: the run's identity, workspace, and each task's projection —
// model, thinking level, the prompt (capped), the write declaration,
// and for every file carried into the prompt its path, byte count and
// SHA-256, never its content. The record's directory is created if
// missing, the record file is opened for append, and the complete line
// including its newline reaches the file in one Write call before Start
// returns, so a run killed at any later instant still leaves its start
// line on disk.
//
// The record deliberately does not carry the identity of the process
// the worker starts: the worker is an implementation detail of one
// run's execution, invisible to the record's readers, so a later
// cleanup feature must search records by workspace and time window
// instead. Records are never deleted, not on success and not on
// failure.
func Start(dir string, startedAt time.Time, workspace string, tasks []run.Task) (*Recorder, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// The run id embeds the process id so two runs starting in the same
	// second cannot collide: the timestamp's finest unit is the second,
	// and the process id separates the runs that share it.
	runID := startedAt.UTC().Format("20060102T150405Z") + "-" + strconv.Itoa(os.Getpid())
	file, err := os.OpenFile(filepath.Join(dir, runID+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	line, err := json.Marshal(startLine{
		SchemaVersion: schemaVersion,
		Event:         "start",
		RunID:         runID,
		StartedAt:     startedAt.UTC().Format(time.RFC3339),
		Workspace:     workspace,
		PID:           os.Getpid(),
		Tasks:         projectTasks(tasks),
	})
	if err != nil {
		file.Close()
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
	line := workerLine{
		SchemaVersion: schemaVersion,
		Event:         "worker",
		RunID:         r.runID,
		At:            at.UTC().Format(time.RFC3339),
		WorkerID:      workerID,
		PID:           pid,
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
// that worker launched.
type workerLine struct {
	SchemaVersion int    `json:"schemaVersion"`
	Event         string `json:"event"`
	RunID         string `json:"runId"`
	At            string `json:"at"`
	WorkerID      int    `json:"workerId"`
	PID           int    `json:"pid"`
}

// finishLine is the second and final line of a run record. Result and
// Error are mutually exclusive: the run returned exactly one of them.
type finishLine struct {
	SchemaVersion int         `json:"schemaVersion"`
	Event         string      `json:"event"`
	RunID         string      `json:"runId"`
	FinishedAt    string      `json:"finishedAt"`
	Result        *run.Result `json:"result,omitempty"`
	Error         string      `json:"error,omitempty"`
}

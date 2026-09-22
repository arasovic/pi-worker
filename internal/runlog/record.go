// Package runlog writes the on-disk record of one run while the run is
// in flight. pi-worker can be killed without warning — the caller hits
// Ctrl-C, the run hits its timeout, the terminal closes, or the
// supervising process is killed outright — and on some of those paths no
// code of ours runs afterwards, so no signal handler can save the run.
// The only design that survives that is a record written as the run
// progresses and left on disk: the start line is written before the run
// starts, one worker line per started worker and one descendant line
// per recorded descendant process while the run is in flight, the
// finish line after the run returns, and a record whose finish line
// never arrived is how a later reader learns the run was interrupted.
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
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/arasovic/pi-worker/internal/config"
	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/run"
	"github.com/shirou/gopsutil/v4/process"
)

// schemaVersion is the run-record document version. It is the record's
// own version, independent of the run result's schemaVersion: the two
// documents evolve separately.
const schemaVersion = 1

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

// liveDescendants is the private dependency-injection seam behind the
// descendant sweeper: it returns, for each given root identity, the
// identities of that root's live descendants from one process-table
// snapshot. Tests replace it with a scripted answer so the records they
// write stay hermetic; production never does.
var liveDescendants = pi.LiveDescendants

// descendantSweepInterval is how often the recorder looks up each
// recorded worker's descendant processes while the run is alive. One
// lookup of the whole tree costs a few milliseconds, so about once a
// second is cheap; a shorter interval records descendants sooner.
//
// ponytail: a process started and orphaned within one interval is
// missed; shorten the interval or hook process creation if that
// matters.
var descendantSweepInterval = time.Second

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

// Recorder writes the four kinds of lines of one run's record: the
// start line at Start, one worker line per started worker while the run
// is in flight, one descendant line per recorded descendant process,
// and the finish line at Finish. A Recorder is shared
// between the goroutine that called Start and will call Finish and up
// to three worker goroutines calling WorkerProcess concurrently. The
// mutex guards the shared write-error slot and the ordering between a
// worker line and the final write-and-close, exactly as the field
// comment below states; appending to a file opened in append mode
// already keeps each line whole, so no write needs guarding for its
// own sake.
//
// One sweeper goroutine per Recorder watches the recorded workers and
// their descendants while the run is alive, recording every new process
// once through Descendant. It starts with the first worker whose
// creation time is known and Finish stops it before the finish line is
// written, so a descendant line can never follow the finish line.
type Recorder struct {
	file  *os.File
	runID string

	// mu guards the shared write-error slot, the ordering between a
	// worker line and the final write-and-close, and the sweeper state
	// below. Appending to a file opened in append mode already keeps
	// each line whole, so no write needs guarding for its own sake;
	// liveDescendants and Descendant are never called with mu held.
	mu sync.Mutex
	// writeErr is the first write error any worker line saw. It is kept
	// rather than warned about inline because a warning printed from
	// three concurrent workers would interleave with the run's own
	// output; Finish returns it once its own write succeeded, so a
	// dropped worker line still surfaces once, through the warning the
	// CLI already prints.
	writeErr error

	// The sweeper state. roots holds every process the sweeper watches —
	// each worker and every descendant already recorded under it, with
	// the worker id the descendant belongs to. recorded dedupes
	// identities across roots. started and stopped decide whether the
	// goroutine exists and whether it may still record; stop and done
	// carry its shutdown handshake.
	roots    []sweepRoot
	recorded map[pi.ProcessIdentity]bool
	stop     chan struct{}
	done     chan struct{}
	started  bool
	stopped  bool
}

// sweepRoot is one process the sweeper watches: a worker root or a
// descendant already recorded under a worker. The worker id travels
// with the identity so a descendant's own later descendants are still
// attributed to the same worker after the descendant is reparented to
// init.
type sweepRoot struct {
	identity pi.ProcessIdentity
	workerID int
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

// StartWithID writes the start line of one run's record before the
// run begins, using the caller-supplied runID as the record filename
// and every line's runId. It validates runID against startedAt before
// any filesystem mutation: the check rejects malformed or inconsistent
// ids without creating a directory or touching the filesystem. The
// record directory is then created if missing, the record file is
// opened exclusively (O_EXCL) — rejecting any existing entry at that
// path, including a regular file, directory, symlink, or named pipe —
// and the complete start line reaches the file in one Write call
// before StartWithID returns.
//
// The start line's pid and createTime describe the current writer
// process (os.Getpid()), not any PID embedded in the supplied runID.
func StartWithID(dir, runID string, startedAt time.Time, workspace string, tasks []run.Task) (*Recorder, error) {
	if err := ValidateRunID(runID, startedAt); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// The creation-time lookup and the marshalling of the start line
	// happen before the record file exists: everything that can fail
	// or block — the process-table read, the JSON encoding — completes
	// before the open, so a marshal failure leaves no file behind, and
	// the file exists without its start line only for the length of
	// the single Write that follows the open. A lookup failure leaves
	// the createTime key off the start line, exactly as it leaves it
	// off a worker line — the identity is weaker then, never an error.
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
		Tasks:         run.ProjectTasks(tasks),
		CreateTime:    createTime,
	})
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, runID+".jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		file.Close()
		return nil, err
	}
	return &Recorder{file: file, runID: runID, recorded: make(map[pi.ProcessIdentity]bool)}, nil
}

// Start writes the start line of one run's record using the run ID
// that RunID derives from startedAt. It is a compatibility wrapper
// around StartWithID.
func Start(dir string, startedAt time.Time, workspace string, tasks []run.Task) (*Recorder, error) {
	return StartWithID(dir, RunID(startedAt), startedAt, workspace, tasks)
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
	// The sweeper watches this worker only when its identity pair is
	// complete: a line without a creation time cannot name a process
	// to look up. Registration happens after the line is written, so
	// no descendant can be recorded before its worker line, and a
	// stopped recorder never starts a sweeper that would outlive the
	// finish line.
	if createTime > 0 && !r.stopped {
		r.addRootLocked(pi.ProcessIdentity{PID: pid, CreateTime: createTime}, workerID)
	}
}

// addRootLocked appends one root and starts the recorder's single
// sweeper on first use. It must be called with r.mu held.
func (r *Recorder) addRootLocked(identity pi.ProcessIdentity, workerID int) {
	r.roots = append(r.roots, sweepRoot{identity: identity, workerID: workerID})
	if r.started {
		return
	}
	r.started = true
	r.stop = make(chan struct{})
	r.done = make(chan struct{})
	// The seams are read here, in the caller's goroutine, and handed to
	// the sweeper as private values: a sweeper that outlives its
	// Recorder must never read `liveDescendants` or
	// `descendantSweepInterval` while a test replaces them.
	interval := descendantSweepInterval
	lookup := liveDescendants
	go r.sweep(interval, lookup)
}

// sweep is the recorder's single descendant sweeper: on every tick it
// looks up the descendants of every root, records each identity it has
// not recorded yet, and promotes it to a root so its own later
// descendants are found even after it is reparented to init. The
// process-table read and the line write happen with mu released.
func (r *Recorder) sweep(interval time.Duration, lookup func([]pi.ProcessIdentity) [][]pi.ProcessIdentity) {
	defer close(r.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			r.sweepOnce(lookup)
		}
	}
}

// sweepOnce performs one lookup over a snapshot of the roots. The
// snapshot is copied under mu and released before the process table is
// read; each new identity is then re-checked and marked under mu, and
// the descendant line is written after mu is released, because
// Descendant takes mu itself.
func (r *Recorder) sweepOnce(lookup func([]pi.ProcessIdentity) [][]pi.ProcessIdentity) {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	roots := make([]sweepRoot, len(r.roots))
	copy(roots, r.roots)
	r.mu.Unlock()

	found := lookup(rootIdentities(roots))
	if found == nil {
		return
	}
	for i, identities := range found {
		if i >= len(roots) {
			break
		}
		workerID := roots[i].workerID
		for _, identity := range identities {
			r.mu.Lock()
			if r.stopped {
				r.mu.Unlock()
				return
			}
			if r.recorded[identity] {
				r.mu.Unlock()
				continue
			}
			r.recorded[identity] = true
			r.roots = append(r.roots, sweepRoot{identity: identity, workerID: workerID})
			r.mu.Unlock()
			r.Descendant(time.Now(), workerID, identity.PID, identity.CreateTime)
		}
	}
}

// rootIdentities projects a root snapshot to the identities the lookup
// seam takes.
func rootIdentities(roots []sweepRoot) []pi.ProcessIdentity {
	identities := make([]pi.ProcessIdentity, len(roots))
	for i, root := range roots {
		identities[i] = root.identity
	}
	return identities
}

// Descendant appends the descendant line of the run's record: the
// identity of one process descended from a worker, written while the
// run is in flight. The caller passes the process's creation time, so
// no process lookup runs here and the line is one already-known
// identity, unlike WorkerProcess. It appends one line and returns
// nothing; a write failure is kept as the recorder's first write error
// and surfaced once by Finish, never printed from the concurrent worker
// goroutine. Descendant on a nil Recorder is a no-op, like Finish.
func (r *Recorder) Descendant(at time.Time, workerID int, pid int, createTime int64) {
	if r == nil {
		return
	}
	line := workerLine{
		SchemaVersion: schemaVersion,
		Event:         "descendant",
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
	// Stop the sweeper before the finish line. Marking stopped and
	// taking the channels under mu, then closing stop and waiting on
	// done outside it, keeps the wait from blocking a final Descendant:
	// once done is closed no descendant line can follow the finish
	// line.
	r.mu.Lock()
	r.stopped = true
	started := r.started
	stop := r.stop
	done := r.done
	r.mu.Unlock()
	if started {
		close(stop)
		<-done
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

// startLine is the first line of a run record, written before the run
// starts. Tasks carries projections (no raw content) produced by
// run.ProjectTasks.
type startLine struct {
	SchemaVersion int                  `json:"schemaVersion"`
	Event         string               `json:"event"`
	RunID         string               `json:"runId"`
	StartedAt     string               `json:"startedAt"`
	Workspace     string               `json:"workspace"`
	PID           int                  `json:"pid"`
	Tasks         []run.TaskProjection `json:"tasks"`
	// CreateTime is the process creation time of the process that
	// wrote the start line — the pi-worker itself — in milliseconds
	// since the Unix epoch, exactly as gopsutil reports it, for exact
	// equality against a later Process.CreateTime. Absent when the
	// lookup failed at write time, which is also the shape of every
	// record written before this field existed.
	CreateTime int64 `json:"createTime,omitempty"`
}

// workerLine is the line of a run record written while the run is in
// flight, one per started worker, carrying the identity of the process
// that worker launched: the pid paired with the process's creation
// time, the pair being the identity — a pid alone is reused, so it
// cannot name a process on its own. The same struct carries a
// descendant line too: its Event is "descendant" and its pid names one
// process descended from the worker.
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

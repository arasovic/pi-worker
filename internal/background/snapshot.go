// Package background defines the snapshot carried across run
// lifecycle phases — the accepted-state record written before workers
// start, describing tasks, workers, workspace and supervisor identity.
package background

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/arasovic/pi-worker/internal/contracts"
	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/run"
	"github.com/arasovic/pi-worker/internal/runlog"
	"github.com/arasovic/pi-worker/internal/worktree"
)

// SchemaVersion is the current snapshot format version.
const SchemaVersion = 1

// RunState labels one phase of a foreground run.
type RunState string

const (
	RunAccepted  RunState = "accepted"
	RunRunning   RunState = "running"
	RunCompleted RunState = "completed"
	RunPartial   RunState = "partial"
	RunFailed    RunState = "failed"
	RunTimedOut  RunState = "timed-out"
	RunCancelled RunState = "cancelled"
)

// WorkerState describes one worker during the run.
type WorkerState string

const (
	WorkerQueued      WorkerState = "queued"
	WorkerRunning     WorkerState = "running"
	WorkerCompleted   WorkerState = "completed"
	WorkerFailed      WorkerState = "failed"
	WorkerTimedOut    WorkerState = "timed-out"
	WorkerCancelled   WorkerState = "cancelled"
	WorkerUnavailable WorkerState = "unavailable"
	WorkerError       WorkerState = "error"
)

// queueDuration is the fixed budget between acceptance and the moment a
// worker may begin executing a task.
const queueDuration = 15 * time.Minute

// ProcessIdentity is the creator-supervised process observed at launch.
type ProcessIdentity struct {
	PID        int   `json:"pid"`
	CreateTime int64 `json:"createTime"`
}

// outcomeSet reports whether o is one of the defined non-usage Outcome values.
func outcomeSet(o contracts.Outcome) bool {
	switch o {
	case contracts.OutcomeWorkersUnavailable, contracts.OutcomeUndeclaredWrites,
		contracts.OutcomeTaskFailed, contracts.OutcomePartial,
		contracts.OutcomeVerificationFailed, contracts.OutcomeTimeout,
		contracts.OutcomeCancelled, contracts.OutcomeInternalError,
		contracts.OutcomeCompleted:
		return true
	}
	return false
}

// Snapshot is the current persisted lifecycle state for one background run.
type Snapshot struct {
	SchemaVersion int                  `json:"schemaVersion"`
	RunID         string               `json:"runId"`
	State         RunState             `json:"state"`
	Terminal      bool                 `json:"terminal"`
	AcceptedAt    time.Time            `json:"acceptedAt"`
	UpdatedAt     time.Time            `json:"updatedAt"`
	Workspace     string               `json:"workspace"`
	Worktree      *worktree.Prepared   `json:"worktree,omitempty"`
	Supervisor    ProcessIdentity      `json:"supervisor"`
	Workers       []WorkerSnapshot     `json:"workers"`
	Status        *contracts.RunStatus `json:"status,omitempty"`
	Outcome       *contracts.Outcome   `json:"outcome,omitempty"`
	Result        *run.Result          `json:"result,omitempty"`
}

// WorkerSnapshot is one worker's role inside a snapshot.
type WorkerSnapshot struct {
	WorkerID         int                `json:"workerId"`
	State            WorkerState        `json:"state"`
	AcceptedAt       time.Time          `json:"acceptedAt"`
	QueueDeadline    time.Time          `json:"queueDeadline"`
	StartedAt        *time.Time         `json:"startedAt,omitempty"`
	FinishedAt       *time.Time         `json:"finishedAt,omitempty"`
	ExecutionTimeout string             `json:"executionTimeout"`
	Task             run.TaskProjection `json:"task"`
	Process          *ProcessIdentity   `json:"process,omitempty"`
	Result           *pi.WorkerResult   `json:"result,omitempty"`
}

// Validate checks that every field is structurally consistent.
// It never mutates s.
func (s Snapshot) Validate() error {
	var errs []string

	// Top-level schema and time fields.
	if s.SchemaVersion != SchemaVersion {
		errs = append(errs, "schemaVersion must be 1")
	}
	if s.AcceptedAt.IsZero() {
		errs = append(errs, "acceptedAt must not be zero")
	} else {
		if s.AcceptedAt.Location() != time.UTC {
			errs = append(errs, "acceptedAt must be in UTC")
		}
		if err := runlog.ValidateRunID(s.RunID, s.AcceptedAt); err != nil {
			errs = append(errs, fmt.Sprintf("runId: %v", err))
		}
	}
	if s.UpdatedAt.IsZero() {
		errs = append(errs, "updatedAt must not be zero")
	}
	if s.UpdatedAt.Location() != time.UTC {
		errs = append(errs, "updatedAt must be in UTC")
	}
	if !s.UpdatedAt.After(s.AcceptedAt) && !s.UpdatedAt.Equal(s.AcceptedAt) {
		errs = append(errs, "updatedAt must be >= acceptedAt")
	}

	// Worktree validation (optional).
	if s.Worktree != nil {
		wt := *s.Worktree
		if !worktree.ValidName(wt.Name) {
			errs = append(errs, "worktree name is invalid")
		}
		if wt.Branch != "run/"+wt.Name {
			errs = append(errs, "worktree branch must be run/<name>")
		}
		if wt.Path == "" {
			errs = append(errs, "worktree path must not be empty")
		}
		if wt.Head == "" {
			errs = append(errs, "worktree head must not be empty")
		}
	}

	// State.
	if !validRunState(s.State) {
		errs = append(errs, "state is not a defined run state")
	}

	// Workspace.
	if s.Workspace == "" {
		errs = append(errs, "workspace must not be empty")
	}

	// Supervisor.
	if s.Supervisor.PID <= 0 || s.Supervisor.CreateTime <= 0 {
		errs = append(errs, "supervisor pid and createTime must be positive")
	}

	// Workers slice and IDs.
	if len(s.Workers) == 0 {
		errs = append(errs, "at least one worker required")
	}
	if len(s.Workers) > run.MaxTasks {
		errs = append(errs, fmt.Sprintf("too many workers: at most %d allowed, got %d", run.MaxTasks, len(s.Workers)))
	}
	for i, w := range s.Workers {
		wantID := i + 1
		if w.WorkerID != wantID {
			errs = append(errs, fmt.Sprintf("worker[%d] id=%d, want %d", i, w.WorkerID, wantID))
		}
	}

	// Lifecycle constraints on top-level status/outcome/result.
	switch s.State {
	case RunAccepted:
		if s.Terminal {
			errs = append(errs, "accepted: terminal must be false")
		}
		if s.Status != nil {
			errs = append(errs, "accepted: status must be nil")
		}
		if s.Outcome != nil {
			errs = append(errs, "accepted: outcome must be nil")
		}
		if s.Result != nil {
			errs = append(errs, "accepted: result must be nil")
		}
		// Every worker must be queued in accepted state.
		for _, w := range s.Workers {
			if w.State != WorkerQueued {
				errs = append(errs, fmt.Sprintf("worker[%d]: must be queued in accepted", w.WorkerID))
			}
		}
		for _, w := range s.Workers {
			if w.StartedAt != nil {
				errs = append(errs, fmt.Sprintf("worker[%d]: startedAt must be nil in accepted", w.WorkerID))
			}
			if w.FinishedAt != nil {
				errs = append(errs, fmt.Sprintf("worker[%d]: finishedAt must be nil in accepted", w.WorkerID))
			}
			if w.Process != nil {
				errs = append(errs, fmt.Sprintf("worker[%d]: process must be nil in accepted", w.WorkerID))
			}
			if w.Result != nil {
				errs = append(errs, fmt.Sprintf("worker[%d]: result must be nil in accepted", w.WorkerID))
			}
		}
	case RunRunning:
		if s.Terminal {
			errs = append(errs, "running: terminal must be false")
		}
		if s.Status != nil {
			errs = append(errs, "running: status must be nil")
		}
		if s.Outcome != nil {
			errs = append(errs, "running: outcome must be nil")
		}
		if s.Result != nil {
			errs = append(errs, "running: result must be nil")
		}
		anyRunningOrTerminal := false
		for _, w := range s.Workers {
			if w.State == WorkerRunning || w.State.isTerminalWorkerState() {
				anyRunningOrTerminal = true
			}
		}
		if !anyRunningOrTerminal {
			errs = append(errs, "running: at least one worker must be running or terminal")
		}
	default:
		// Terminal run states: completed/partial/failed/timed-out/cancelled.
		if !s.Terminal {
			errs = append(errs, fmt.Sprintf("%s: terminal must be true", s.State))
		}
		if s.Status == nil {
			errs = append(errs, fmt.Sprintf("%s: status must not be nil", s.State))
		} else if *s.Status != contracts.RunStatus(s.State) {
			errs = append(errs, fmt.Sprintf("%s: status must equal state", s.State))
		}
		if s.Outcome == nil || !outcomeSet(*s.Outcome) {
			errs = append(errs, fmt.Sprintf("%s: outcome must be a defined non-usage value", s.State))
		}
		if s.Result != nil {
			if string(s.Result.Status) != string(s.State) {
				errs = append(errs, "result.status mismatch with snapshot state")
			}
			if s.Outcome != nil && string(s.Result.Outcome) != string(*s.Outcome) {
				errs = append(errs, "result.outcome mismatch with snapshot outcome")
			}
		}
		for _, w := range s.Workers {
			if !w.State.isTerminalWorkerState() {
				errs = append(errs, fmt.Sprintf("worker[%d]: must be terminal", w.WorkerID))
			}
		}
	}

	// Per-worker validation.
	for _, w := range s.Workers {
		if ve := validateWorker(w, s.AcceptedAt, s.UpdatedAt); len(ve) > 0 {
			errs = append(errs, ve...)
		}
	}

	// Multi-worker cross-checks: writes must be declared and paths must not overlap.
	if len(s.Workers) > 1 {
		workerPaths := make(map[int][]string)
		for _, w := range s.Workers {
			if !w.Task.WritesDeclared {
				errs = append(errs, fmt.Sprintf("worker[%d]: writesDeclared must be true when multiple workers", w.WorkerID))
			}
			workerPaths[w.WorkerID-1] = w.Task.Writes
		}
		for idA, pathsA := range workerPaths {
			for idB, pathsB := range workerPaths {
				if idB <= idA {
					continue
				}
				for _, pA := range pathsA {
					for _, pB := range pathsB {
						if pathsOverlap(pA, pB) {
							errs = append(errs, fmt.Sprintf("worker[%d]/worker[%d]: paths %q and %q overlap", idA+1, idB+1, pA, pB))
						}
					}
				}
			}
		}
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// validRunState reports whether rs is one of the defined RunState constants.
func validRunState(rs RunState) bool {
	switch rs {
	case RunAccepted, RunRunning, RunCompleted, RunPartial,
		RunFailed, RunTimedOut, RunCancelled:
		return true
	}
	return false
}

// isTerminalWorkerState reports whether ws is one of the terminal WorkerState values.
func (ws WorkerState) isTerminalWorkerState() bool {
	switch ws {
	case WorkerCompleted, WorkerFailed, WorkerTimedOut,
		WorkerCancelled, WorkerUnavailable, WorkerError:
		return true
	}
	return false
}

// validateWorker checks per-worker rules.  Returns a slice of messages
// so callers can accumulate into a single error string.
func validateWorker(w WorkerSnapshot, snapAccepted, snapUpdated time.Time) []string {
	var errs []string

	// acceptedAt equals snapshot acceptedAt; queueDeadline equals
	// acceptedAt + 15 minutes.
	if !w.AcceptedAt.Equal(snapAccepted) {
		errs = append(errs, fmt.Sprintf("worker[%d]: acceptedAt must equal snapshot acceptedAt", w.WorkerID))
	}
	if w.AcceptedAt.Location() != time.UTC {
		errs = append(errs, fmt.Sprintf("worker[%d]: acceptedAt must be in UTC", w.WorkerID))
	}
	deadline := snapAccepted.Add(queueDuration)
	if !w.QueueDeadline.Equal(deadline) {
		errs = append(errs, fmt.Sprintf("worker[%d]: queueDeadline must be acceptedAt + 15m", w.WorkerID))
	}

	// Validate location/offset (canonical time.UTC). No zero-field panic.
	if w.QueueDeadline.Location() != time.UTC {
		errs = append(errs, fmt.Sprintf("worker[%d]: queueDeadline must be in UTC", w.WorkerID))
	}
	if w.StartedAt != nil && w.StartedAt.Location() != time.UTC {
		errs = append(errs, fmt.Sprintf("worker[%d]: startedAt must be in UTC", w.WorkerID))
	}
	if w.FinishedAt != nil && w.FinishedAt.Location() != time.UTC {
		errs = append(errs, fmt.Sprintf("worker[%d]: finishedAt must be in UTC", w.WorkerID))
	}
	// Order: startedAt >= acceptedAt (when present).
	if w.StartedAt != nil && (!w.StartedAt.After(w.AcceptedAt) && !w.StartedAt.Equal(w.AcceptedAt)) {
		errs = append(errs, fmt.Sprintf("worker[%d]: startedAt must be >= acceptedAt", w.WorkerID))
	}
	// Order: finishedAt >= startedAt (when both present); else finishedAt >= acceptedAt.
	if w.FinishedAt != nil {
		cmpBase := w.AcceptedAt
		if w.StartedAt != nil {
			cmpBase = *w.StartedAt
		}
		if !w.FinishedAt.After(cmpBase) && !w.FinishedAt.Equal(cmpBase) {
			errs = append(errs, fmt.Sprintf("worker[%d]: finishedAt must be >= acceptedAt", w.WorkerID))
		}
	}
	// finishedAt <= updatedAt.
	if w.FinishedAt != nil {
		if !snapUpdated.After(*w.FinishedAt) && !snapUpdated.Equal(*w.FinishedAt) {
			errs = append(errs, fmt.Sprintf("worker[%d]: finishedAt must be <= updatedAt", w.WorkerID))
		}
	}

	// executionTimeout.
	if w.ExecutionTimeout == "" {
		errs = append(errs, fmt.Sprintf("worker[%d]: executionTimeout must not be empty", w.WorkerID))
	} else {
		d, err := time.ParseDuration(w.ExecutionTimeout)
		if err != nil {
			errs = append(errs, fmt.Sprintf("worker[%d]: executionTimeout does not parse: %v", w.WorkerID, err))
		} else if d <= 0 {
			errs = append(errs, fmt.Sprintf("worker[%d]: executionTimeout must be positive", w.WorkerID))
		}
	}

	// Task projection.
	if te := validateTaskProjection(w.Task, w.WorkerID); len(te) > 0 {
		errs = append(errs, te...)
	}

	// Process identity (optional but positive when present).
	if w.Process != nil {
		if w.Process.PID <= 0 || w.Process.CreateTime <= 0 {
			errs = append(errs, fmt.Sprintf("worker[%d]: process pid and createTime must be positive", w.WorkerID))
		}
	}

	// Worker lifecycle: queued / running / terminal transitions.
	switch w.State {
	case WorkerQueued:
		if w.StartedAt != nil || w.FinishedAt != nil || w.Process != nil || w.Result != nil {
			errs = append(errs, fmt.Sprintf("worker[%d]: queued worker must not have startedAt, finishedAt, process, or result", w.WorkerID))
		}
	case WorkerRunning:
		if w.StartedAt == nil {
			errs = append(errs, fmt.Sprintf("worker[%d]: running requires startedAt", w.WorkerID))
		}
		if w.Process == nil {
			errs = append(errs, fmt.Sprintf("worker[%d]: running requires process", w.WorkerID))
		}
		if w.FinishedAt != nil {
			errs = append(errs, fmt.Sprintf("worker[%d]: running must not have finishedAt", w.WorkerID))
		}
		if w.Result != nil {
			errs = append(errs, fmt.Sprintf("worker[%d]: running must not have result", w.WorkerID))
		}
	default:
		if !w.State.isTerminalWorkerState() {
			errs = append(errs, fmt.Sprintf("worker[%d]: unknown worker state %q", w.WorkerID, w.State))
		} else {
			if w.FinishedAt == nil {
				errs = append(errs, fmt.Sprintf("worker[%d]: terminal worker requires finishedAt", w.WorkerID))
			}
			if w.Result != nil && w.Result.Status != string(w.State) {
				errs = append(errs, fmt.Sprintf("worker[%d]: result.status must equal worker state", w.WorkerID))
			}
		}
	}

	return errs
}

// validateTaskProjection validates the task projection fields.
func validateTaskProjection(t run.TaskProjection, workerID int) []string {
	var errs []string

	// Model: provider/id shape with non-empty halves.
	if t.Model == "" {
		errs = append(errs, fmt.Sprintf("worker[%d]: model must not be empty", workerID))
	} else {
		provider, id, ok := splitModel(t.Model)
		if !ok || provider == "" || id == "" {
			errs = append(errs, fmt.Sprintf("worker[%d]: model must be provider/id with non-empty halves", workerID))
		}
	}

	// ThinkingLevel: empty or valid.
	if t.ThinkingLevel != "" {
		if _, ok := pi.ParseThinkingLevel(t.ThinkingLevel); !ok {
			errs = append(errs, fmt.Sprintf("worker[%d]: thinkingLevel is not a valid Pi thinking level", workerID))
		}
	}

	// Prompt: valid UTF-8 and <= promptCap bytes.
	if !utf8.ValidString(t.Prompt) {
		errs = append(errs, fmt.Sprintf("worker[%d]: prompt is not valid UTF-8", workerID))
	}
	const maxPromptBytes = 4096
	if len(t.Prompt) > maxPromptBytes {
		errs = append(errs, fmt.Sprintf("worker[%d]: prompt exceeds %d bytes (%d)", workerID, maxPromptBytes, len(t.Prompt)))
	}
	// promptTruncated should not permit over-limit bytes.
	if t.PromptTruncated && len(t.Prompt) > maxPromptBytes {
		errs = append(errs, fmt.Sprintf("worker[%d]: truncated prompt still exceeds limit", workerID))
	}

	// Writes.
	if !t.WritesDeclared {
		if len(t.Writes) > 0 {
			errs = append(errs, fmt.Sprintf("worker[%d]: writes must be empty when writesDeclared=false", workerID))
		}
	} else {
		seen := make(map[string]bool, len(t.Writes))
		for _, wp := range t.Writes {
			if strings.TrimSpace(wp) == "" {
				errs = append(errs, fmt.Sprintf("worker[%d]: write path must not be empty", workerID))
				continue
			}
			clean := filepath.Clean(wp)
			if clean == "." {
				errs = append(errs, fmt.Sprintf("worker[%d]: write path must not resolve to .", workerID))
			}
			if clean != wp {
				errs = append(errs, fmt.Sprintf("worker[%d]: write path must be clean", workerID))
			}
			if filepath.IsAbs(wp) {
				errs = append(errs, fmt.Sprintf("worker[%d]: write path must be relative", workerID))
			}
			if hasDotDot(wp) {
				errs = append(errs, fmt.Sprintf("worker[%d]: write path must not contain .. segment", workerID))
			}
			if seen[wp] {
				errs = append(errs, fmt.Sprintf("worker[%d]: duplicate write path %q", workerID, wp))
			}
			seen[wp] = true
		}
	}

	// Data items.
	dataSeen := make(map[string]bool, len(t.Data))
	for _, df := range t.Data {
		if df.Path == "" {
			errs = append(errs, fmt.Sprintf("worker[%d]: data file path must not be empty", workerID))
		}
		if df.Bytes < 0 {
			errs = append(errs, fmt.Sprintf("worker[%d]: data file byteCount must be >= 0", workerID))
		}
		if len(df.SHA256) != 64 {
			errs = append(errs, fmt.Sprintf("worker[%d]: data file sha256 must be exactly 64 hex characters", workerID))
		} else if !isLowerHex(df.SHA256) {
			errs = append(errs, fmt.Sprintf("worker[%d]: data file sha256 must be lowercase hex", workerID))
		}
		if dataSeen[df.Path] {
			errs = append(errs, fmt.Sprintf("worker[%d]: duplicate data path %q", workerID, df.Path))
		}
		dataSeen[df.Path] = true
	}

	return errs
}

// splitModel splits "provider/id" and returns (provider, id, true).
func splitModel(m string) (provider, id string, ok bool) {
	idx := strings.IndexByte(m, '/')
	if idx < 1 {
		return "", "", false
	}
	return m[:idx], m[idx+1:], true
}

// isLowerHex reports whether s consists solely of [0-9a-f].
func isLowerHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// hasDotDot reports whether p contains a bare ".." path segment.
func hasDotDot(p string) bool {
	segs := strings.Split(p, "/")
	for _, s := range segs {
		if s == ".." {
			return true
		}
	}
	return false
}

// pathsOverlap reports whether a and b overlap as path sets on
// a path-segment boundary. "src/a" and "src/ab" are non-overlapping,
// but "src/a" overlaps with "src/a/b" or "src/a/file.txt".
func pathsOverlap(a, b string) bool {
	segsA := strings.Split(filepath.Clean(a), "/")
	segsB := strings.Split(filepath.Clean(b), "/")
	minLen := len(segsA)
	if len(segsB) < minLen {
		minLen = len(segsB)
	}
	for i := 0; i < minLen; i++ {
		if segsA[i] != segsB[i] {
			return false
		}
	}
	if len(segsA) == len(segsB) {
		return true // identical paths
	}
	// One is a strict ancestor prefix at segment boundary.
	return true
}

// NewSnapshot builds an accepted, non-terminal Snapshot ready for
// serialization and transport.
func NewSnapshot(
	runID string,
	acceptedAt time.Time,
	workspace string,
	supervisor ProcessIdentity,
	tasks []run.Task,
	executionTimeout time.Duration,
	prepared *worktree.Prepared,
) (Snapshot, error) {
	if acceptedAt.IsZero() {
		return Snapshot{}, fmt.Errorf("acceptedAt must not be zero")
	}
	if err := runlog.ValidateRunID(runID, acceptedAt); err != nil {
		return Snapshot{}, fmt.Errorf("validate runId: %w", err)
	}
	if workspace == "" {
		return Snapshot{}, fmt.Errorf("workspace is required")
	}
	if supervisor.PID <= 0 {
		return Snapshot{}, fmt.Errorf("supervisor: pid must be positive")
	}
	if supervisor.CreateTime <= 0 {
		return Snapshot{}, fmt.Errorf("supervisor: createTime must be positive")
	}
	if len(tasks) == 0 {
		return Snapshot{}, fmt.Errorf("at least one task required, got %d", len(tasks))
	}
	if len(tasks) > run.MaxTasks {
		return Snapshot{}, fmt.Errorf("at most %d tasks supported, got %d", run.MaxTasks, len(tasks))
	}
	if executionTimeout <= 0 {
		return Snapshot{}, fmt.Errorf("executionTimeout must be positive")
	}

	// Normalize acceptedAt to UTC, truncated to whole seconds (matches
	// ValidateRunID / StartWithID precision).
	acceptedAt = acceptedAt.UTC().Truncate(time.Second)

	// Copy prepared so later caller mutation cannot alter the snapshot.
	var wt *worktree.Prepared
	if prepared != nil {
		cp := *prepared
		wt = &cp
	}

	projected := run.ProjectTasks(tasks)
	deadline := acceptedAt.Add(queueDuration)

	workers := make([]WorkerSnapshot, len(projected))
	for i, proj := range projected {
		workers[i] = WorkerSnapshot{
			WorkerID:         i + 1,
			State:            WorkerQueued,
			AcceptedAt:       acceptedAt,
			QueueDeadline:    deadline,
			ExecutionTimeout: executionTimeout.String(),
			Task:             proj,
		}
	}

	snap := Snapshot{
		SchemaVersion: SchemaVersion,
		RunID:         runID,
		State:         RunAccepted,
		Terminal:      false,
		AcceptedAt:    acceptedAt,
		UpdatedAt:     acceptedAt,
		Workspace:     workspace,
		Worktree:      wt,
		Supervisor:    supervisor,
		Workers:       workers,
	}
	if err := snap.Validate(); err != nil {
		return Snapshot{}, fmt.Errorf("validate snapshot after construction: %w", err)
	}
	return snap, nil
}

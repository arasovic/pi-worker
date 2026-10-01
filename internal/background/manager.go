package background

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/arasovic/pi-worker/internal/config"
	"github.com/arasovic/pi-worker/internal/run"
	"github.com/arasovic/pi-worker/internal/runlog"
	"github.com/arasovic/pi-worker/internal/worktree"
)

// defaultStartWait bounds how long Start waits for the supervisor's answer
// to the acceptance handshake when the caller named no wait of its own. It
// is a wait budget only: expiring it never cancels, kills, or otherwise
// touches a supervisor, and a Start that ran out of it had already been
// answered, because the handshake is all Start ever waits on.
const defaultStartWait = 2 * time.Minute

// worktreeCleanupWait bounds the removal of the one worktree a start
// prepared, so a start that was not accepted cannot hang on a cleanup whose
// own context was the reason it was not accepted.
const worktreeCleanupWait = 30 * time.Second

// waitPollInterval is how often Wait reads one run's snapshot while it
// waits. It is a private test seam, so a test can observe a wait that ends
// before its next poll; production never changes it.
var waitPollInterval = 50 * time.Millisecond

// ownerAlive decides whether a run's supervisor is still there; tests
// replace it to control the liveness check.
var ownerAlive = runlog.OwnerAlive

// errSupervisorRejected is the reason of a start the supervisor answered
// with a rejection. The handoff already joined the bounded rejection reason
// into the error it returned, so this sentinel only classifies the outcome
// for errors.Is; it never replaces that reason.
var errSupervisorRejected = errors.New("background manager start: supervisor did not accept the run")

// Manager starts background runs, reports what one run's latest durable
// Snapshot says, and waits for a run to become terminal. It is the whole
// starter-side surface: it owns where a run's state lives — the snapshot
// Store and the admission Gate, resolved against the same roots by every
// start and every read — and the machine-wide worker limit those two share.
//
// Manager is a concrete type with no interface and no scheduler of its own.
// The scheduler is the supervisor process: a started run is executed by a
// detached role child that nothing here keeps a handle on, so one Manager
// answers about a run it started and about a run it never saw, and losing
// that Manager loses nothing durable.
type Manager struct {
	root            string
	legacyRoot      string
	admissionRoot   string
	maxModelWorkers int
}

// NewManager returns the Manager that writes run state under root, reads it
// there and, for a run an older version started, under legacyRoot, and admits
// at most maxModelWorkers live model workers through the admission Gate at
// admissionRoot. An empty root or admission root falls back to the default
// location of that kind of state inside the user configuration tree: the run
// directory [DefaultRoot] names, and the admission directory beside the
// configuration file a foreground run opens its own gate from, so an empty
// admission root never names the filesystem root. An empty legacyRoot reads
// no older runs. The limit must be positive, because a Gate with no live slot
// could admit nothing. Nothing here touches the filesystem: constructing a
// Manager creates no directory and no state.
func NewManager(root, legacyRoot, admissionRoot string, maxModelWorkers int) (*Manager, error) {
	if root == "" {
		defaultRoot, err := DefaultRoot()
		if err != nil {
			return nil, fmt.Errorf("background manager: resolve the user configuration directory: %w", err)
		}
		root = defaultRoot
	}
	if admissionRoot == "" {
		userDir, err := config.UserDir()
		if err != nil {
			return nil, fmt.Errorf("background manager: resolve the user configuration directory: %w", err)
		}
		admissionRoot = filepath.Join(userDir, "admission")
	}
	if maxModelWorkers <= 0 {
		return nil, fmt.Errorf("background manager: maxModelWorkers must be positive, got %d", maxModelWorkers)
	}
	return &Manager{root: root, legacyRoot: legacyRoot, admissionRoot: admissionRoot, maxModelWorkers: maxModelWorkers}, nil
}

// rootOf returns the root holding runID's run directory: the legacy root when
// the run directory exists only there — a run an older version started —
// and the Manager's own root otherwise, so a run found nowhere is reported
// against the root new runs are written to. A run never moves between roots,
// so one answer holds for the whole of a call.
func (m *Manager) rootOf(runID string) string {
	if m.legacyRoot == "" {
		return m.root
	}
	if _, err := os.Lstat(filepath.Join(m.root, runID)); !errors.Is(err, fs.ErrNotExist) {
		return m.root
	}
	if _, err := os.Lstat(filepath.Join(m.legacyRoot, runID)); err != nil {
		return m.root
	}
	return m.legacyRoot
}

// StartOptions is what a caller brings to Start: the tasks and the
// workspace to run them in, the verification command and the execution
// timeout every worker is held to, the Pi executable the workers drive, and
// the name of a private checkout to work in instead of the caller's current
// directory.
type StartOptions struct {
	Tasks     []run.Task
	Workspace string
	Verify    []string
	// ExecutionTimeout is one worker's bound — the same value a foreground
	// run's --timeout carries. It must be positive: a snapshot cannot
	// express a worker without one.
	ExecutionTimeout time.Duration
	// PiExecutable is the workers' Pi, never the pi-worker binary that
	// hosts the role processes.
	PiExecutable string
	// WorktreeName, when set, is the name of the managed private checkout
	// the run works in: created by Start under <root>/.pi-worker/worktrees
	// on branch run/<name>, and the workspace the run is recorded against.
	WorktreeName string
	// RoleExecutable names the program a supervisor is spawned from. Empty
	// means this program, which is what production always wants: the
	// supervisor is the same binary re-executed with a private role token.
	// A caller whose own program does not dispatch role tokens — a test
	// binary — names the built one here instead.
	RoleExecutable string
	// QueueWait bounds how long Start waits for the acceptance handshake.
	// Unset or non-positive means the default bound.
	QueueWait time.Duration
	Debug     bool
	// NoTranscript keeps no Pi session transcript in the run's directory.
	NoTranscript bool

	// removeWorktree is the private seam the unaccepted-start cleanup uses
	// instead of worktree.RemoveUntouched, so a test can observe exactly
	// which worktree a start that was not accepted gave back, and can make
	// that removal fail.
	removeWorktree worktreeRemover
}

// worktreeRemover is the shape of worktree.RemoveUntouched: it removes one
// prepared checkout only while nothing has touched it.
type worktreeRemover func(ctx context.Context, cwd string, expected worktree.Prepared) error

func (o StartOptions) resolver() worktreeRemover {
	if o.removeWorktree != nil {
		return o.removeWorktree
	}
	return worktree.RemoveUntouched
}

// StartedRun is the run Start handed to a supervisor: its identity and the
// accepted Snapshot that supervisor persisted before it answered.
type StartedRun struct {
	// RunID identifies the run and names its snapshot directory. It is the
	// argument Status and Wait take.
	RunID string
	// Snapshot is the accepted snapshot the supervisor itself wrote, bound
	// to the exact detached process now running the tasks. It is the
	// acceptance answer, not the current state: the run was still going
	// when Start returned.
	Snapshot Snapshot
}

// validateStartOptions rejects the Start arguments the request validator
// cannot see: the Manager's own roots and limit, and the workspace, which is
// the directory a prepared worktree would be created beside. Every other
// field — tasks, verification argv, execution timeout, Pi executable — is
// validated by the request itself, before any process exists, and a start
// whose request cannot be encoded never creates anything.
func (m *Manager) validateStartOptions(opts StartOptions) error {
	if m == nil {
		return errors.New("background manager start: nil manager")
	}
	if m.root == "" || m.admissionRoot == "" || m.maxModelWorkers <= 0 {
		return errors.New("background manager start: manager carries no roots or no positive worker limit")
	}
	if opts.Workspace == "" {
		return errors.New("background manager start: workspace is required")
	}
	if opts.WorktreeName != "" && !worktree.ValidName(opts.WorktreeName) {
		return fmt.Errorf("background manager start: invalid worktree name %q: use 1 to 64 characters of lowercase letters, digits and hyphens, starting and ending with a letter or digit", opts.WorktreeName)
	}
	return nil
}

// DebugLogPath names the file a run started with debug on writes its debug
// lines to. It exists only once a worker of that run has started, and only
// when the run was started with debug on.
func (m *Manager) DebugLogPath(runID string) string {
	return debugLogPath(m.rootOf(runID), runID)
}

// Status returns the latest durable Snapshot of one run. It waits for
// nothing — not for a terminal snapshot, not for the supervisor to answer,
// not for the run to move — so the Snapshot it reports of a run in flight
// is the state that was durable at the moment it read, and the same call
// made again reports whatever has become durable since. When the snapshot
// it reads is non-terminal and its owner is gone — runlog.OwnerAlive, the
// owner lock when the run directory has one, the recorded supervisor
// process otherwise — it
// reads once more: a terminal second read is the supervisor having finished
// between the read and the check and is returned as the answer, and
// otherwise the second snapshot is returned with a
// SupervisorUnavailableError, because the run was interrupted and will not
// finish.
//
// The run need not have been started by this Manager, and no run need have
// existed at all: the error of those cases comes from the Store, which
// refuses to read anything that is not exactly one valid snapshot of the run
// it was asked for.
func (m *Manager) Status(runID string) (Snapshot, error) {
	root := m.rootOf(runID)
	store, err := NewStore(root)
	if err != nil {
		return Snapshot{}, fmt.Errorf("background manager status (%s): construct store: %w", runID, err)
	}
	snap, err := store.Load(runID)
	if err != nil {
		return Snapshot{}, fmt.Errorf("background manager status (%s): %w", runID, err)
	}
	if snap.Terminal || runlog.OwnerAlive(filepath.Join(root, runID), snap.Supervisor.PID, snap.Supervisor.CreateTime) {
		return snap, nil
	}
	snap, err = store.Load(runID)
	if err != nil {
		return Snapshot{}, fmt.Errorf("background manager status (%s): %w", runID, err)
	}
	if snap.Terminal {
		return snap, nil
	}
	return snap, &SupervisorUnavailableError{PID: snap.Supervisor.PID, Reason: errors.New("recorded supervisor process is gone")}
}

// Wait blocks until the named run's durable Snapshot is terminal and returns
// exactly that Snapshot. It never cancels, never kills, and never touches
// the running supervisor: reading is all it does to a run. When a
// non-terminal snapshot's recorded supervisor process is gone, Wait reads
// once more and, unless the second read is terminal, returns the second
// snapshot with a SupervisorUnavailableError at once, without waiting out
// the bound, because the run was interrupted and will not finish.
//
// When the bound it was given arrives first, Wait returns the latest
// non-terminal Snapshot it read together with the context error that ended
// the wait — that Snapshot is a live run's state, never a run that failed,
// and only the error says the wait is over. Every other read failure, a run
// with no snapshot yet or a snapshot that cannot be decoded, is reported
// with the zero Snapshot, so a run whose state nobody can read is never
// passed off as a run that is merely still going.
func (m *Manager) Wait(ctx context.Context, runID string, timeout time.Duration) (Snapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	if m == nil {
		return Snapshot{}, fmt.Errorf("background manager wait (%s): nil manager", runID)
	}
	root := m.rootOf(runID)
	store, err := NewStore(root)
	if err != nil {
		return Snapshot{}, fmt.Errorf("background manager wait (%s): construct store: %w", runID, err)
	}
	if _, err := runlog.ParseRunID(runID); err != nil {
		return Snapshot{}, fmt.Errorf("background manager wait (%s): %w", runID, err)
	}

	var latest Snapshot
	for {
		// Read before consulting cancellation: a terminal snapshot that is
		// already durable is this call's answer however it was started, and
		// a run nobody can read is reported that way even to a caller that
		// has just given up on waiting.
		snap, loadErr := store.Load(runID)
		if loadErr != nil {
			// The read failure is the answer, whatever else is also true of
			// this call — including that its bound is already spent. A wait
			// that ran out without ever reading anything has no state to
			// report, and the reason it cannot report one is the read, not the
			// bound: handing back a zero Snapshot with the context error would
			// print a run with no identity as a run that is merely still going.
			return Snapshot{}, fmt.Errorf("background manager wait (%s): %w", runID, loadErr)
		}
		if snap.Terminal {
			return snap, nil
		}
		if !ownerAlive(filepath.Join(root, runID), snap.Supervisor.PID, snap.Supervisor.CreateTime) {
			snap, loadErr = store.Load(runID)
			if loadErr != nil {
				return Snapshot{}, fmt.Errorf("background manager wait (%s): %w", runID, loadErr)
			}
			if snap.Terminal {
				return snap, nil
			}
			return snap, &SupervisorUnavailableError{PID: snap.Supervisor.PID, Reason: errors.New("recorded supervisor process is gone")}
		}
		latest = snap

		interval := waitPollInterval
		if deadline, ok := ctx.Deadline(); ok {
			if remaining := time.Until(deadline); remaining <= 0 {
				// The bound is already spent and the run is still going: the
				// latest state is the whole answer.
				return latest, context.DeadlineExceeded
			} else if remaining < interval {
				interval = remaining
			}
		} else if err := ctx.Err(); err != nil {
			// Cancelled with no bound to wait to.
			return latest, err
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return latest, ctx.Err()
		case <-timer.C:
		}
	}
}

// joinStartErrors joins the non-nil errors in order, returning the single
// error itself when there is only one, so a start failure that needs no
// cleanup keeps the exact error the handoff reported.
func joinStartErrors(primary, cleanupDiag error) error {
	if cleanupDiag == nil {
		return primary
	}
	return errors.Join(primary, cleanupDiag)
}

//go:build darwin || linux

package background

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/arasovic/pi-worker/internal/runlog"
	"golang.org/x/sys/unix"
)

// supervisorSignal is the one signal-delivery seam. Production sends only
// SIGTERM; tests can replace it without ever risking an unrelated process.
var supervisorSignal = func(pid int) error { return unix.Kill(pid, unix.SIGTERM) }

// Cancel reads one durable snapshot and, only for a non-terminal snapshot
// whose owner lock is held — or whose run directory has no owner lock — and
// whose supervisor identity still matches the process table, sends SIGTERM.
// A free lock, or one that cannot be probed, sends nothing.
// It never writes a snapshot and never waits for either the supervisor or the
// run. A terminal snapshot is returned without consulting the process table.
func (m *Manager) Cancel(runID string) (Snapshot, error) {
	if m == nil {
		return Snapshot{}, fmt.Errorf("background manager cancel (%s): nil manager", runID)
	}
	root := m.rootOf(runID)
	store, err := NewStore(root)
	if err != nil {
		return Snapshot{}, fmt.Errorf("background manager cancel (%s): construct store: %w", runID, err)
	}
	snap, err := store.Load(runID)
	if err != nil {
		return Snapshot{}, fmt.Errorf("background manager cancel (%s): %w", runID, err)
	}
	if snap.Terminal {
		return snap, nil
	}

	// ponytail: the lock is probed, then the pid is signalled; an owner that
	// exits in between can leave the pid to be reused before the signal. The
	// creation-time match below narrows that window; closing it needs a
	// signal that names the process rather than its number.
	switch runlog.ProbeOwnerLock(filepath.Join(root, runID)) {
	case runlog.LockFree:
		return snap, &SupervisorUnavailableError{PID: snap.Supervisor.PID, Reason: errors.New("owner lock is free")}
	case runlog.LockUnknown:
		return snap, &SupervisorUnavailableError{PID: snap.Supervisor.PID, Reason: errors.New("owner lock cannot be probed")}
	}
	created, err := supervisorPidCreateTime(snap.Supervisor.PID)
	if err != nil {
		return snap, &SupervisorUnavailableError{PID: snap.Supervisor.PID, Reason: err}
	}
	if created != snap.Supervisor.CreateTime {
		return snap, &SupervisorUnavailableError{
			PID:    snap.Supervisor.PID,
			Reason: fmt.Errorf("creation time %d does not match recorded %d", created, snap.Supervisor.CreateTime),
		}
	}
	if err := supervisorSignal(snap.Supervisor.PID); err != nil {
		if errors.Is(err, unix.ESRCH) {
			return snap, &SupervisorUnavailableError{PID: snap.Supervisor.PID, Reason: err}
		}
		return snap, &SupervisorSignalError{PID: snap.Supervisor.PID, Err: err}
	}
	return snap, nil
}

//go:build darwin || linux

package background

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// supervisorSignal is the one signal-delivery seam. Production sends only
// SIGTERM; tests can replace it without ever risking an unrelated process.
var supervisorSignal = func(pid int) error { return unix.Kill(pid, unix.SIGTERM) }

// Cancel reads one durable snapshot and, only for a non-terminal snapshot
// whose supervisor identity still matches the process table, sends SIGTERM.
// It never writes a snapshot and never waits for either the supervisor or the
// run. A terminal snapshot is returned without consulting the process table.
func (m *Manager) Cancel(runID string) (Snapshot, error) {
	if m == nil {
		return Snapshot{}, fmt.Errorf("background manager cancel (%s): nil manager", runID)
	}
	store, err := NewStore(m.root)
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

//go:build !darwin && !linux

package background

import "fmt"

// Cancel cannot signal a supervisor on a platform that cannot host one.
func (m *Manager) Cancel(runID string) (Snapshot, error) {
	return Snapshot{}, fmt.Errorf("cancel (%s): background runs are not supported on this platform", runID)
}

//go:build !darwin && !linux

package background

import (
	"context"
	"fmt"
)

// Start refuses the run before it mints an identity, prepares a worktree,
// or builds a request: the run it would describe needs a supervisor role
// process, and no role process can start here. The refusal is the same one
// dispatchSupervisorRole answers the role token with, and it is reached the
// same way — by a caller who has nothing to hand a run over to.
func (m *Manager) Start(context.Context, StartOptions) (StartedRun, error) {
	return StartedRun{}, fmt.Errorf("background manager start: %w", errRoleProcessUnsupported)
}

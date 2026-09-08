//go:build !darwin && !linux

package background

import (
	"context"
	"fmt"
)

// supervisorStartHandoffResult mirrors the Unix type name on unsupported
// platforms. It is never produced with accepted true because no handoff
// can run there.
type supervisorStartHandoffResult struct {
	snapshot Snapshot
	accepted bool
}

// supervisorStartProcessFunc mirrors the Unix process-start function type
// on unsupported platforms; it is never invoked.
type supervisorStartProcessFunc func(executable string, r role) (*roleProcess, error)

// startSupervisorHandoff returns the explicit unsupported error before
// any process or pipe is created on unsupported platforms.
func startSupervisorHandoff(context.Context, string, supervisorStartRequest) (supervisorStartHandoffResult, error) {
	return supervisorStartHandoffResult{}, fmt.Errorf("start supervisor handoff: %w", errRoleProcessUnsupported)
}

// startSupervisorHandoffWithProcess behaves identically to
// startSupervisorHandoff on unsupported platforms: the process starter is
// never consulted.
func startSupervisorHandoffWithProcess(context.Context, string, supervisorStartRequest, supervisorStartProcessFunc) (supervisorStartHandoffResult, error) {
	return supervisorStartHandoffResult{}, fmt.Errorf("start supervisor handoff: %w", errRoleProcessUnsupported)
}

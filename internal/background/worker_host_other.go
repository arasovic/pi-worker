//go:build !darwin && !linux

package background

import (
	"context"
	"fmt"

	"github.com/arasovic/pi-worker/internal/pi"
)

// workerHostPlatformSupported is false wherever role processes cannot
// start: workerHostAdapter.Run answers such platforms with the existing
// errRoleProcessUnsupported result before any request validation (see
// worker_host_unix.go for the supported-platform twin).
const workerHostPlatformSupported = false

// execute returns the existing unsupported role-process result without
// launching a process or creating any pipe on unsupported platforms:
// the child host handler cannot run where role processes cannot start,
// and no host is spawned for it. workerHostAdapter.Run already answers
// unsupported platforms before request validation, so this method is
// reached only if Run's tail is called directly; it exists so Run
// compiles on every platform.
func (a *workerHostAdapter) execute(_ context.Context, req pi.WorkerRequest, _ []byte) pi.WorkerResult {
	return pi.WorkerResult{Model: req.Model, Status: pi.StatusUnavailable, Error: fmt.Sprintf("start worker host: %v", errRoleProcessUnsupported)}
}

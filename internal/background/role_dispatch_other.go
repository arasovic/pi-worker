//go:build !darwin && !linux

package background

import (
	"fmt"
	"io"
)

// dispatchWorkerHostRole rejects the worker-host role token where no role
// process can start: the child transport descriptors no adapter could
// have inherited here, so no exchange can run and the process says so and
// fails.
func dispatchWorkerHostRole(stderr io.Writer) (bool, int) {
	fmt.Fprintf(stderr, "pi-worker: %s role: %v\n", roleWorkerHost, errRoleProcessUnsupported)
	return true, roleExitUnsupportedPlatform
}

//go:build darwin || linux

package background

import (
	"fmt"
	"io"
)

// dispatchWorkerHostRole runs one private worker-host child over the
// fixed descriptors the adapter handed it and reports the role exit code
// for how far it got. The child transport ends belong to the role from
// here on: openChildRolePipes wraps fds 3, 4 and 5, and receiveWorkerHost
// closes all three on every return, including its own failures, so no
// path below leaves a descriptor open. Nothing is written to stdout: a
// role child has no CLI output, and its parent reads the response
// descriptor and the exit code.
func dispatchWorkerHostRole(stderr io.Writer) (bool, int) {
	pipes, err := openChildRolePipes(roleWorkerHost)
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: open %s role pipes: %v\n", roleWorkerHost, err)
		return true, roleExitPipesUnavailable
	}
	if _, err := receiveWorkerHost(pipes); err != nil {
		fmt.Fprintf(stderr, "pi-worker: receive %s: %v\n", roleWorkerHost, err)
		return true, roleExitExchangeFailed
	}
	return true, roleExitSucceeded
}

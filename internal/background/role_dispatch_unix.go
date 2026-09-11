//go:build darwin || linux

package background

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
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

// dispatchSupervisorRole runs one private supervisor child over the fixed
// descriptors the starter handed it and reports the role exit code for how
// far it got. receiveSupervisorStart closes both supervisor transport ends
// on every return, including its own failures, so no path below leaves one
// of them open. Nothing is written to stdout: a role child has no CLI
// output, and its parent reads the response descriptor and the exit code.
func dispatchSupervisorRole(stderr io.Writer) (bool, int) {
	pipes, err := openChildRolePipes(roleSupervisor)
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: open %s role pipes: %v\n", roleSupervisor, err)
		return true, roleExitPipesUnavailable
	}
	result, err := receiveSupervisorStart(pipes)
	if err != nil {
		// An accepted result may still carry a pipe-close diagnostic: the run
		// it describes is sound regardless, so the diagnostic is reported and
		// the accepted path continues below.
		fmt.Fprintf(stderr, "pi-worker: receive %s: %v\n", roleSupervisor, err)
	}
	if !result.accepted {
		// A rejection that still carries a preparation is one whose own
		// rollback stayed incomplete; this caller owns it and finishes the
		// rollback exactly once before failing.
		if result.preparation != nil {
			if rollbackErr := result.preparation.rollback(); rollbackErr != nil {
				fmt.Fprintf(stderr, "pi-worker: roll back %s preparation: %v\n", roleSupervisor, rollbackErr)
			}
		}
		return true, roleExitExchangeFailed
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: resolve own executable for %s: %v\n", roleSupervisor, err)
		return true, roleExitExecutableUnavailable
	}
	// No deadline here: the queue deadline is the accepted one, and the
	// execution timeout is applied per worker inside the run controller.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// Keep interception installed through runAcceptedRun's terminal snapshot
	// write: a second SIGTERM must not restore the default disposition and
	// kill the supervisor while that write is in flight.
	defer stop()
	if err := runAcceptedRun(ctx, executable, result); err != nil {
		fmt.Fprintf(stderr, "pi-worker: run accepted %s run: %v\n", roleSupervisor, err)
		return true, roleExitRunFailed
	}
	return true, roleExitSucceeded
}

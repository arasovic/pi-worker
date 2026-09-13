//go:build darwin || linux

package background

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// runSupervisorDispatchSignalChild is the opt-in TestMain child mode that
// proves a SIGTERM delivered during the supervisor start exchange does not
// kill the child. It replaces receiveSupervisorStartFunc with a seam that
// SIGTERMs this very process, sleeps 200ms so the signal is certainly
// delivered while the exchange is still in flight, and then returns an
// unaccepted result with no preparation. dispatchSupervisorRole must reach
// its ordinary rejection exit instead of dying by the signal's default
// disposition. The function never returns: it exits with the dispatch
// code. TestMain enters this mode before opening any wrapper for the
// inherited role descriptors, because the production dispatch opens its own
// wrappers for those same fds.
func runSupervisorDispatchSignalChild() {
	receiveSupervisorStartFunc = func(*childRolePipes) (supervisorStartResult, error) {
		if err := unix.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			fmt.Fprintf(os.Stderr, "signal own process: %v\n", err)
			os.Exit(84)
		}
		time.Sleep(200 * time.Millisecond)
		return supervisorStartResult{}, nil
	}
	_, code := dispatchSupervisorRole(os.Stderr)
	os.Exit(code)
}

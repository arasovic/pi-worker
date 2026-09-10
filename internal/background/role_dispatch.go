package background

import (
	"fmt"
	"io"
)

// Role-child exit codes. A role child's observable result is its exit
// code plus the diagnostics on its stderr, so each way a child can fail
// gets its own code and no code is reused for a different failure. They
// stay below 128: a signal death is reported as 128+signal, and a role
// code must never be mistaken for one.
const (
	// roleExitSucceeded is the code of a child that completed its
	// exchange and put its terminal result on the wire.
	roleExitSucceeded = 0
	// roleExitPipesUnavailable reports that the inherited role transport
	// descriptors could not be opened, so no exchange was ever possible.
	roleExitPipesUnavailable = 94
	// roleExitExchangeFailed reports that the child opened its transport
	// and then failed the exchange itself.
	roleExitExchangeFailed = 71
	// roleExitRoleNotDispatched reports a private role token the binary
	// recognises but does not run yet, so a parent can tell it from a
	// child that started its exchange and failed it.
	roleExitRoleNotDispatched = 70
	// roleExitUnsupportedPlatform reports a private role token on a
	// platform where no role process can start at all. It is outside
	// every code any role child or CLI path already uses, so a parent
	// can tell "this platform can never host a role" from a child that
	// failed its own exchange.
	roleExitUnsupportedPlatform = 78
)

// DispatchRole runs a private role child when name is a private role
// token, reporting whether it handled the argument and the exit code the
// process must use. The main binary consults it before any command
// dispatch: a role child is started with the role token as its only
// argument, so recognising that token here is what lets the production
// binary run as a child of its own adapter. An argument that is not a
// role token is reported as unhandled with code 0 and runs nothing, so
// the caller continues with its normal command dispatch.
func DispatchRole(name string, stderr io.Writer) (handled bool, code int) {
	switch r := role(name); r {
	case roleSupervisor:
		// The supervisor child half answers a start handshake and hands
		// off to a scheduling loop the product does not have yet, so a
		// child that accepted here would report acceptance from a
		// supervisor that immediately exits and leaves its run with no
		// one to schedule it. The token is recognised and refused; the
		// exchange is never started.
		fmt.Fprintf(stderr, "pi-worker: %s role is not dispatched yet\n", roleSupervisor)
		return true, roleExitRoleNotDispatched
	case roleWorkerHost:
		return dispatchWorkerHostRole(stderr)
	default:
		return false, roleExitSucceeded
	}
}

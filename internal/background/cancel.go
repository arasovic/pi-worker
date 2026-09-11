package background

import "fmt"

// SupervisorUnavailableError reports that a live snapshot's supervisor cannot
// be confirmed from the current process table. No signal has been sent.
type SupervisorUnavailableError struct {
	PID    int
	Reason error
}

func (e *SupervisorUnavailableError) Error() string {
	if e == nil {
		return "background supervisor is no longer there"
	}
	return fmt.Sprintf("background supervisor is no longer there (pid %d: %v)", e.PID, e.Reason)
}

// SupervisorSignalError reports a failure to deliver SIGTERM after the
// supervisor identity matched. The snapshot remains untouched.
type SupervisorSignalError struct {
	PID int
	Err error
}

func (e *SupervisorSignalError) Error() string {
	if e == nil {
		return "background supervisor signal failed"
	}
	return fmt.Sprintf("background supervisor signal failed to pid %d: %v", e.PID, e.Err)
}

func (e *SupervisorSignalError) Unwrap() error { return e.Err }

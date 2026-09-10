package run

import (
	"github.com/arasovic/pi-worker/internal/contracts"
	"github.com/arasovic/pi-worker/internal/pi"
)

// RunFailure resolves which (status, error kind) pair describes the
// aggregate run result, in the documented precedence order. It is the
// single place that decision is made, over the fields the controller
// filled: the CLI derives its outcome word and exit code from what it
// returns, and so does any other caller that has to name the outcome of
// a Result it never ran itself — a background supervisor persisting a
// terminal snapshot, for one. Both derive from what it returns, so they
// cannot describe different things. The codes follow the documented
// precedence order: run-outcome codes first (5, 7, 8, 9, and the
// readiness 3), then the write-check policy code 4, then the
// verification code 6, then completion's 0. A no-success run exits 3
// when every worker was unavailable and 9 when any worker reported an
// internal error; partial runs stay 5. The run status field always
// describes worker outcomes only.
func RunFailure(result Result) (contracts.RunStatus, *contracts.RunError) {
	switch result.Status {
	case contracts.RunTimedOut:
		return contracts.RunTimedOut, &contracts.RunError{Kind: contracts.ErrorTimeout}
	case contracts.RunCancelled:
		return contracts.RunCancelled, &contracts.RunError{Kind: contracts.ErrorCancellation}
	}
	// Partial and failed runs keep their run-outcome codes before any
	// check is considered: a run that did not complete is answered by
	// its outcome, not by the checks that run on every terminal status.
	if result.Status != contracts.RunCompleted {
		hasSuccess := false
		hasError := false
		allUnavailable := true
		for _, worker := range result.Workers {
			switch worker.Status {
			case pi.StatusCompleted:
				hasSuccess = true
			case pi.StatusError:
				hasError = true
				allUnavailable = false
			case pi.StatusUnavailable:
			default:
				allUnavailable = false
			}
		}
		switch {
		case !hasSuccess && allUnavailable:
			return result.Status, &contracts.RunError{Kind: contracts.ErrorReadiness}
		case !hasSuccess && hasError:
			return result.Status, &contracts.RunError{Kind: contracts.ErrorInternal}
		default:
			return result.Status, &contracts.RunError{Kind: contracts.ErrorTask}
		}
	}
	// Completed only. The write contract outranks the quality signal: a
	// run that wrote outside its declared scope has breached the
	// contract the caller relied on to bound it, and whether its tests
	// pass is secondary information the result document carries either
	// way. Contract breach outranks quality signal. Only a verdict with
	// undeclared paths exits 4: a skipped check never does — a skip
	// means the question could not be answered, and answering
	// "violation" would be a lie — and a clean verdict never does.
	if result.Writes != nil && result.Writes.Skipped == "" && result.Writes.UndeclaredCount > 0 {
		return contracts.RunCompleted, &contracts.RunError{Kind: contracts.ErrorPolicy}
	}
	if result.Verification != nil && result.Verification.ExitCode != 0 {
		return contracts.RunCompleted, &contracts.RunError{Kind: contracts.ErrorVerification}
	}
	return contracts.RunCompleted, nil
}

package contracts

import "testing"

func TestExitCodeMapping(t *testing.T) {
	tests := []struct {
		name        string
		status      RunStatus
		runError    *RunError
		want        int
		wantOutcome Outcome
	}{
		{name: "usage error", status: RunFailed, runError: &RunError{Kind: ErrorUsage}, want: 2, wantOutcome: OutcomeUsage},
		{name: "readiness error", status: RunFailed, runError: &RunError{Kind: ErrorReadiness}, want: 3, wantOutcome: OutcomeWorkersUnavailable},
		{name: "policy error", status: RunFailed, runError: &RunError{Kind: ErrorPolicy}, want: 4, wantOutcome: OutcomeUndeclaredWrites},
		{name: "completed with policy kind", status: RunCompleted, runError: &RunError{Kind: ErrorPolicy}, want: 4, wantOutcome: OutcomeUndeclaredWrites},
		{name: "task error", status: RunFailed, runError: &RunError{Kind: ErrorTask}, want: 5, wantOutcome: OutcomeTaskFailed},
		{name: "partial task error", status: RunPartial, runError: &RunError{Kind: ErrorTask}, want: 5, wantOutcome: OutcomePartial},
		{name: "verification error", status: RunFailed, runError: &RunError{Kind: ErrorVerification}, want: 6, wantOutcome: OutcomeVerificationFailed},
		{name: "completed with verification kind", status: RunCompleted, runError: &RunError{Kind: ErrorVerification}, want: 6, wantOutcome: OutcomeVerificationFailed},
		{name: "timeout error", status: RunFailed, runError: &RunError{Kind: ErrorTimeout}, want: 7, wantOutcome: OutcomeTimeout},
		{name: "timed out with timeout kind", status: RunTimedOut, runError: &RunError{Kind: ErrorTimeout}, want: 7, wantOutcome: OutcomeTimeout},
		{name: "cancellation error", status: RunFailed, runError: &RunError{Kind: ErrorCancellation}, want: 8, wantOutcome: OutcomeCancelled},
		{name: "cancelled with cancellation kind", status: RunCancelled, runError: &RunError{Kind: ErrorCancellation}, want: 8, wantOutcome: OutcomeCancelled},
		{name: "internal error", status: RunFailed, runError: &RunError{Kind: ErrorInternal}, want: 9, wantOutcome: OutcomeInternalError},
		{name: "usage error overrides completed", status: RunCompleted, runError: &RunError{Kind: ErrorUsage}, want: 2, wantOutcome: OutcomeUsage},
		{name: "internal error overrides completed", status: RunCompleted, runError: &RunError{Kind: ErrorInternal}, want: 9, wantOutcome: OutcomeInternalError},
		{name: "completed", status: RunCompleted, want: 0, wantOutcome: OutcomeCompleted},
		{name: "partial", status: RunPartial, want: 5, wantOutcome: OutcomePartial},
		{name: "failed", status: RunFailed, want: 5, wantOutcome: OutcomeTaskFailed},
		{name: "timed out", status: RunTimedOut, want: 7, wantOutcome: OutcomeTimeout},
		{name: "cancelled", status: RunCancelled, want: 8, wantOutcome: OutcomeCancelled},
		{name: "unknown error kind", status: RunFailed, runError: &RunError{Kind: "other"}, want: 9, wantOutcome: OutcomeInternalError},
		{name: "unknown status", status: "other", want: 9, wantOutcome: OutcomeInternalError},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ExitCode(test.status, test.runError); got != test.want {
				t.Fatalf("exit = %d, want %d", got, test.want)
			}
			if got := RunOutcome(test.status, test.runError); got != test.wantOutcome {
				t.Fatalf("outcome = %q, want %q", got, test.wantOutcome)
			}
		})
	}
}

// TestOutcomeExitCodeInvertsRunOutcome requires that the exit code read back
// from an outcome word equals the exit code of the (status, error kind) pair
// the word was derived from, for every pair the enums define. A reader that
// only has a stored outcome must exit the way the run that stored it would.
func TestOutcomeExitCodeInvertsRunOutcome(t *testing.T) {
	statuses := []RunStatus{RunCompleted, RunPartial, RunFailed, RunTimedOut, RunCancelled}
	runErrors := []*RunError{nil}
	for _, kind := range []ErrorKind{ErrorUsage, ErrorReadiness, ErrorPolicy, ErrorTask, ErrorVerification, ErrorTimeout, ErrorCancellation, ErrorInternal} {
		runErrors = append(runErrors, &RunError{Kind: kind})
	}
	for _, status := range statuses {
		for _, runError := range runErrors {
			outcome := RunOutcome(status, runError)
			if got, want := OutcomeExitCode(outcome), ExitCode(status, runError); got != want {
				t.Errorf("status %q, error %+v: OutcomeExitCode(%q) = %d, want %d", status, runError, outcome, got, want)
			}
		}
	}
}

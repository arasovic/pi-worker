package contracts

import "testing"

func TestExitCodeMapping(t *testing.T) {
	tests := []struct {
		name     string
		status   RunStatus
		runError *RunError
		want     int
	}{
		{name: "usage error", status: RunFailed, runError: &RunError{Kind: ErrorUsage}, want: 2},
		{name: "readiness error", status: RunFailed, runError: &RunError{Kind: ErrorReadiness}, want: 3},
		{name: "policy error", status: RunFailed, runError: &RunError{Kind: ErrorPolicy}, want: 4},
		{name: "task error", status: RunFailed, runError: &RunError{Kind: ErrorTask}, want: 5},
		{name: "verification error", status: RunFailed, runError: &RunError{Kind: ErrorVerification}, want: 6},
		{name: "timeout error", status: RunFailed, runError: &RunError{Kind: ErrorTimeout}, want: 7},
		{name: "cancellation error", status: RunFailed, runError: &RunError{Kind: ErrorCancellation}, want: 8},
		{name: "internal error", status: RunFailed, runError: &RunError{Kind: ErrorInternal}, want: 9},
		{name: "usage error overrides completed", status: RunCompleted, runError: &RunError{Kind: ErrorUsage}, want: 2},
		{name: "internal error overrides completed", status: RunCompleted, runError: &RunError{Kind: ErrorInternal}, want: 9},
		{name: "completed", status: RunCompleted, want: 0},
		{name: "partial", status: RunPartial, want: 5},
		{name: "failed", status: RunFailed, want: 5},
		{name: "timed out", status: RunTimedOut, want: 7},
		{name: "cancelled", status: RunCancelled, want: 8},
		{name: "unknown error kind", status: RunFailed, runError: &RunError{Kind: "other"}, want: 9},
		{name: "unknown status", status: "other", want: 9},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ExitCode(test.status, test.runError); got != test.want {
				t.Fatalf("exit = %d, want %d", got, test.want)
			}
		})
	}
}

package contracts

type ErrorKind string

const (
	ErrorUsage        ErrorKind = "usage"
	ErrorReadiness    ErrorKind = "readiness"
	ErrorPolicy       ErrorKind = "policy"
	ErrorTask         ErrorKind = "task"
	ErrorVerification ErrorKind = "verification"
	ErrorTimeout      ErrorKind = "timeout"
	ErrorCancellation ErrorKind = "cancellation"
	ErrorInternal     ErrorKind = "internal"
)

type RunError struct {
	Kind    ErrorKind `json:"kind"`
	Message string    `json:"message"`
}

func ExitCode(status RunStatus, runError *RunError) int {
	if runError != nil {
		switch runError.Kind {
		case ErrorUsage:
			return 2
		case ErrorReadiness:
			return 3
		case ErrorPolicy:
			return 4
		case ErrorTask:
			return 5
		case ErrorVerification:
			return 6
		case ErrorTimeout:
			return 7
		case ErrorCancellation:
			return 8
		case ErrorInternal:
			return 9
		default:
			return 9
		}
	}

	switch status {
	case RunCompleted:
		return 0
	case RunPartial, RunFailed:
		return 5
	case RunTimedOut:
		return 7
	case RunCancelled:
		return 8
	default:
		return 9
	}
}

type Outcome string

const (
	// OutcomeUsage exists so RunOutcome is total over the same domain as
	// ExitCode and the two switches stay comparable line by line. It
	// never reaches a result document: a usage error fails before a run
	// exists.
	OutcomeUsage              Outcome = "usage"
	OutcomeWorkersUnavailable Outcome = "workers-unavailable"
	OutcomeUndeclaredWrites   Outcome = "undeclared-writes"
	OutcomeTaskFailed         Outcome = "task-failed"
	OutcomePartial            Outcome = "partial"
	OutcomeVerificationFailed Outcome = "verification-failed"
	OutcomeTimeout            Outcome = "timeout"
	OutcomeCancelled          Outcome = "cancelled"
	OutcomeInternalError      Outcome = "internal-error"
	OutcomeCompleted          Outcome = "completed"
)

// RunOutcome mirrors ExitCode branch for branch, with one deliberate
// difference: exit code 5 covers both a failed run and a partial one,
// and the word separates them.
func RunOutcome(status RunStatus, runError *RunError) Outcome {
	if runError != nil {
		switch runError.Kind {
		case ErrorUsage:
			return OutcomeUsage
		case ErrorReadiness:
			return OutcomeWorkersUnavailable
		case ErrorPolicy:
			return OutcomeUndeclaredWrites
		case ErrorTask:
			if status == RunPartial {
				return OutcomePartial
			}
			return OutcomeTaskFailed
		case ErrorVerification:
			return OutcomeVerificationFailed
		case ErrorTimeout:
			return OutcomeTimeout
		case ErrorCancellation:
			return OutcomeCancelled
		case ErrorInternal:
			return OutcomeInternalError
		default:
			return OutcomeInternalError
		}
	}

	switch status {
	case RunCompleted:
		return OutcomeCompleted
	case RunPartial:
		return OutcomePartial
	case RunFailed:
		return OutcomeTaskFailed
	case RunTimedOut:
		return OutcomeTimeout
	case RunCancelled:
		return OutcomeCancelled
	default:
		return OutcomeInternalError
	}
}

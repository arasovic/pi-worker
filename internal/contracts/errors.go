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

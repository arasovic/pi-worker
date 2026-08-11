package contracts

const SchemaVersion = 1

type RunStatus string

const (
	RunCompleted RunStatus = "completed"
	RunPartial   RunStatus = "partial"
	RunFailed    RunStatus = "failed"
	RunTimedOut  RunStatus = "timed-out"
	RunCancelled RunStatus = "cancelled"
)

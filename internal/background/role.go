package background

import "errors"

var errRoleProcessUnsupported = errors.New("process role communication unsupported")

// Role identifies one side in the supervisor–worker process relationship.
type role string

const (
	roleSupervisor role = "__pi-worker-background-supervisor"
	roleWorkerHost role = "__pi-worker-background-worker-host"
)

// validRole reports whether r is one of the defined role constants.
func validRole(r role) bool {
	return r == roleSupervisor || r == roleWorkerHost
}

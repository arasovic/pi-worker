package admission

import (
	"fmt"
	"os"

	"github.com/shirou/gopsutil/v4/process"
)

// ownerIdentity holds the process-level identity of the current
// admission owner: its PID and its process-table creation time as
// reported by the operating system. Both values must be positive
// for the identity to be usable; a zero or negative value is treated
// as absent or invalid.
type ownerIdentity struct {
	PID        int
	CreateTime int64
}

// ownerState is the classification of a stored owner identity
// against the live process table.
type ownerState int

const (
	ownerSame      ownerState = iota // same process
	ownerStale                       // different or absent process
	ownerUncertain                   // lookup failed or invalid identity
)

// pidExists reports whether a process with the given PID exists in
// the process table. It is a seam for tests.
var pidExists = defaultPidExists

func defaultPidExists(pid int) (bool, error) {
	return process.PidExists(int32(pid))
}

// pidCreateTime returns the creation time of the process with the
// given PID from the process table. It is a seam for tests.
var pidCreateTime = defaultPidCreateTime

func defaultPidCreateTime(pid int) (int64, error) {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return 0, err
	}
	return p.CreateTime()
}

// ownerGetpid is the private seam for the PID of the current process.
// Tests replace it; production uses os.Getpid.
var ownerGetpid = os.Getpid

// currentOwner returns the identity of the current process. It reads
// the PID from the operating system and looks up the process creation
// time via pidCreateTime. A zero or negative PID, or a zero or
// negative creation time, or a process-table error all cause an error
// rather than inventing an identity.
func currentOwner() (ownerIdentity, error) {
	pid := ownerGetpid()
	if pid <= 0 {
		return ownerIdentity{}, fmt.Errorf("current owner: invalid pid %d", pid)
	}
	createTime, err := pidCreateTime(pid)
	if err != nil {
		return ownerIdentity{}, fmt.Errorf("current owner: lookup pid %d: %w", pid, err)
	}
	if createTime <= 0 {
		return ownerIdentity{}, fmt.Errorf("current owner: invalid createTime %d for pid %d", createTime, pid)
	}
	return ownerIdentity{PID: pid, CreateTime: createTime}, nil
}

// ownerStatus inspects the process table to classify stored against
// the live owner:
//
//   - ownerSame:      the stored PID exists with the same creation time.
//   - ownerStale:     the stored PID is absent from the process table,
//     or it exists but was created at a different time.
//   - ownerUncertain: stored identity is invalid (non-positive PID or
//     CreateTime), or a process-table lookup failed.
func ownerStatus(stored ownerIdentity) ownerState {
	if stored.PID <= 0 || stored.CreateTime <= 0 {
		return ownerUncertain
	}

	exists, err := pidExists(stored.PID)
	if err != nil {
		return ownerUncertain
	}
	if !exists {
		return ownerStale
	}

	liveCreate, err := pidCreateTime(stored.PID)
	if err != nil || liveCreate <= 0 {
		return ownerUncertain
	}
	if liveCreate == stored.CreateTime {
		return ownerSame
	}
	return ownerStale
}

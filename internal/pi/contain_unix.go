//go:build darwin || linux

package pi

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// childContainment places the pi child in its own process group. Terminating
// the group removes descendants that retain the inherited group, and a
// descendant sweep (descendants_unix.go) additionally terminates ordinary
// descendants that moved to another process group, such as commands started
// by Pi's built-in bash tool. The group is created atomically by the exec
// path (Setpgid), so there is no post-start setup window. This is best-effort
// lifecycle recovery, not containment: a process that deliberately calls
// setsid and reparents itself away can escape, and a descendant spawned
// during the teardown sweep itself may too; both are outside v0's guarantee.
type childContainment struct{}

func newChildContainment() (*childContainment, error) {
	return &childContainment{}, nil
}

// preStart configures the command to start the child as the leader of a new
// process group.
func (c *childContainment) preStart(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}

// assign is a no-op on Unix: the process group is established by the exec
// path before the child runs any code.
func (c *childContainment) assign(*os.Process) error { return nil }

// terminate kills the child's entire process group and then best-effort
// terminates ordinary descendants that left the group. The pid must never be
// <= 1: a non-positive pid would signal the caller's own group (0) or every
// process on the system (-1), so the caller's fallback kills the direct
// child instead.
//
// The descendant tree is snapshotted before the group kill: once the direct
// child dies, surviving descendants are reparented away and their lineage to
// it is no longer visible to any sweep. Every snapshot target is killed with
// a creation-time identity check, so pid reuse cannot redirect a kill. A
// failure to inspect the table is swallowed (the sweep degrades to the group
// kill alone), and a descendant that spawns its own child after the snapshot
// is outside the sweep.
func (c *childContainment) terminate(pid int) error {
	if pid <= 1 {
		return fmt.Errorf("refusing to signal the process group of pid %d", pid)
	}
	targets := inspectDescendantTargets(int32(pid))
	err := syscall.Kill(-pid, syscall.SIGKILL)
	killDescendantTargets(targets)
	return err
}

// snapshotDescendants captures the best-effort Unix-only lineage identity for
// descendants of pid, before the descendant can be reparented away.
func (c *childContainment) snapshotDescendants(pid int) any {
	if pid <= 1 {
		return nil
	}
	return inspectDescendantTargets(int32(pid))
}

// terminateDescendants applies the pre-close lineage-based sweep for best-effort
// cleanup after a normal root exit.
func (c *childContainment) terminateDescendants(targets any) {
	targetList, ok := targets.([]descendantTarget)
	if !ok {
		return
	}
	killDescendantTargets(targetList)
}

// close is a no-op on Unix: there is no OS resource to release.
func (c *childContainment) close() error { return nil }

//go:build darwin || linux

package pi

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/shirou/gopsutil/v4/process"
)

// childContainment records the Pi root's creation-time identity and starts it
// in a separate process group. Teardown never signals that numeric group:
// after Wait reaps the root, the group id can be reused by an unrelated
// process. Instead it kills the direct child through os.Process and performs a
// creation-time-verified descendant sweep. This is best-effort lifecycle
// recovery, not containment: a process that deliberately reparents itself
// before the snapshot can escape, and a descendant spawned during the sweep
// may too; both are outside v0's guarantee.
type childContainment struct {
	root descendantTarget
}

func newChildContainment() (*childContainment, error) {
	return &childContainment{}, nil
}

// preStart configures the command to start the child as the leader of a new
// process group.
func (c *childContainment) preStart(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}

// assign records the stable root identity before Start reports success. If it
// cannot be proven, descendant cleanup cannot safely attribute a future pid
// to this child, so startup fails closed.
func (c *childContainment) assign(proc *os.Process) error {
	if proc == nil || proc.Pid <= 1 {
		return fmt.Errorf("invalid child pid")
	}
	root, err := process.NewProcess(int32(proc.Pid))
	if err != nil {
		return fmt.Errorf("inspect child %d: %w", proc.Pid, err)
	}
	created, err := root.CreateTime()
	if err != nil {
		return fmt.Errorf("inspect child %d creation time: %w", proc.Pid, err)
	}
	c.root = descendantTarget{pid: int32(proc.Pid), createTime: created}
	return nil
}

// terminate snapshots descendants only when the process table still contains
// the exact root identity recorded at startup. os.Process.Kill is safe after a
// concurrent Wait: it returns os.ErrProcessDone instead of signalling a reused
// pid. Every descendant kill is independently creation-time verified.
func (c *childContainment) terminate(proc *os.Process) error {
	if proc == nil || proc.Pid <= 1 || c.root.pid != int32(proc.Pid) {
		return fmt.Errorf("refusing to terminate invalid child identity")
	}
	targets := inspectDescendantTargets(c.root)
	err := proc.Kill()
	killDescendantTargets(targets)
	return err
}

// snapshotDescendants captures the best-effort Unix-only lineage identity for
// descendants of pid, before the descendant can be reparented away.
func (c *childContainment) snapshotDescendants(pid int) any {
	if pid <= 1 || c.root.pid != int32(pid) {
		return nil
	}
	return inspectDescendantTargets(c.root)
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

//go:build !darwin && !linux && !windows

package pi

import (
	"fmt"
	"os"
	"os/exec"
)

// childContainment on unsupported platforms has no process-tree facility:
// preStart leaves the command untouched and terminate always fails, so the
// Process fallback kills only the direct child. The worker still builds and
// runs with the same lifecycle; only the descendant guarantee is absent.
type childContainment struct{}

func newChildContainment() (*childContainment, error) {
	return &childContainment{}, nil
}

func (c *childContainment) preStart(cmd *exec.Cmd) error {
	return nil
}

func (c *childContainment) assign(*os.Process) error { return nil }

func (c *childContainment) terminate(pid int) error {
	return fmt.Errorf("process-tree containment unsupported on this platform")
}

// snapshotDescendants is a no-op where no descendant tracking is available.
func (c *childContainment) snapshotDescendants(_ int) any { return nil }

// terminateDescendants is a no-op where no descendant tracking is available.
func (c *childContainment) terminateDescendants(_ any) {}

func (c *childContainment) close() error { return nil }

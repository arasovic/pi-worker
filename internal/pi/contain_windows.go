//go:build windows

package pi

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// childContainment places the pi child inside a Windows Job Object with
// kill-on-close: every process the child spawns joins the job, and
// terminating or closing the job terminates its members. Setup and assignment
// are required before Start reports success. The child starts before
// assignment because os/exec exposes no suspended-create/resume hook;
// therefore a process created in that short window could escape the job.
// Assignment failure kills and reaps the direct child, but v0 does not claim
// a sandbox or a no-escape guarantee for that window.
type childContainment struct {
	job windows.Handle
}

func newChildContainment() (*childContainment, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("configure job object: %w", err)
	}
	return &childContainment{job: job}, nil
}

// preStart configures the command so the child can be placed in the job:
// the breakaway flag lets the child leave a restrictive parent job when the
// environment permits it, and creation fails closed when it does not.
func (c *childContainment) preStart(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_BREAKAWAY_FROM_JOB}
	return nil
}

// assign binds the started child into the job object. Failure rejects Start;
// the caller immediately kills and reaps the direct child.
func (c *childContainment) assign(proc *os.Process) error {
	handle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(proc.Pid))
	if err != nil {
		return fmt.Errorf("open process %d: %w", proc.Pid, err)
	}
	defer windows.CloseHandle(handle)
	if err := windows.AssignProcessToJobObject(c.job, handle); err != nil {
		return fmt.Errorf("assign process to job object: %w", err)
	}
	return nil
}

// terminate kills every process in the job, including the child and all
// descendants. The pid is not needed: job membership identifies the tree.
func (c *childContainment) terminate(_ int) error {
	return windows.TerminateJobObject(c.job, 1)
}

// snapshotDescendants is a no-op on Windows: the job object already tracks
// descendants reliably, and descendants started in separate sessions still stay in
// the job by construction.
func (c *childContainment) snapshotDescendants(_ int) any { return nil }

// terminateDescendants is a no-op on Windows: process-group leakage is handled
// by the job object boundary.
func (c *childContainment) terminateDescendants(_ any) {}

// close releases the job handle; kill-on-close terminates any process still
// in the job, so a missed or failed terminate cannot leak a descendant.
func (c *childContainment) close() error {
	return windows.CloseHandle(c.job)
}

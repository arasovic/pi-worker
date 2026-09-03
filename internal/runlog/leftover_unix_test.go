//go:build darwin || linux

package runlog

import (
	"os"
	"testing"
)

// TestDefaultLiveProcessesRejectsPIDReusedBetweenGroupAndCreateTimeReads
// scripts a different creation time for this process after the group read.
// The group seam makes the ordering explicit, so the test does not depend
// on real PID-reuse timing. The process-table row is retained as unreadable
// rather than indexed, preserving the sweep's best-effort behavior.
func TestDefaultLiveProcessesRejectsPIDReusedBetweenGroupAndCreateTimeReads(t *testing.T) {
	pid := os.Getpid()
	originalGetpgid := getpgid
	groupRead := false
	getpgid = func(gotPID int) (int, error) {
		if gotPID == pid {
			groupRead = true
		}
		return originalGetpgid(gotPID)
	}
	t.Cleanup(func() { getpgid = originalGetpgid })

	originalPidCreateTime := pidCreateTime
	pidCreateTime = func(gotPID int) (int64, error) {
		if gotPID == pid {
			if !groupRead {
				t.Errorf("creation-time confirmation ran before process-group read")
			}
			return 2000, nil
		}
		return originalPidCreateTime(gotPID)
	}
	t.Cleanup(func() { pidCreateTime = originalPidCreateTime })

	rows, err := defaultLiveProcesses()
	if err != nil {
		t.Fatalf("defaultLiveProcesses: %v", err)
	}

	found := false
	for _, row := range rows {
		if row.pid != pid {
			continue
		}
		found = true
		if !row.unreadable {
			t.Fatalf("defaultLiveProcesses kept mixed identity row: %#v", row)
		}
	}
	if !found {
		t.Fatalf("defaultLiveProcesses did not retain an unreadable row for pid %d", pid)
	}
}

//go:build darwin || linux

package runlog

import (
	"syscall"

	"github.com/shirou/gopsutil/v4/process"
)

// getpgid is a seam for testing the identity check around the syscall.
var getpgid = syscall.Getpgid

// defaultLiveProcesses snapshots the process table once: every live
// process with the process group it belongs to and its creation time,
// the three facts the leftover reader indexes. Each row is inspected
// best-effort — a process that vanishes mid-sweep, whose group cannot
// be read, or whose creation time cannot be read is excluded from the
// indexes and its pid is retained as unreadable. The snapshot still
// succeeds, so one unreadable row can never fail the whole sweep.
func defaultLiveProcesses() ([]liveProcess, error) {
	procs, err := process.Processes()
	if err != nil {
		return nil, err
	}
	table := make([]liveProcess, 0, len(procs))
	for _, p := range procs {
		pid := int(p.Pid)
		created, err := p.CreateTime()
		if err != nil {
			// A creation time that cannot be read leaves this pid
			// unconfirmed, so the caller can skip its whole record.
			table = append(table, liveProcess{pid: pid, unreadable: true})
			continue
		}
		pgid, err := getpgid(pid)
		if err != nil {
			// The process vanished mid-sweep, or the call is not
			// permitted: without a group it cannot be attributed.
			table = append(table, liveProcess{pid: pid, unreadable: true})
			continue
		}
		confirmed, err := pidCreateTime(pid)
		if err != nil || confirmed != created {
			// The pid changed identity during the process-group read,
			// or its identity could not be confirmed: this row mixes
			// facts from different processes and must not be indexed.
			table = append(table, liveProcess{pid: pid, unreadable: true})
			continue
		}
		table = append(table, liveProcess{pid: pid, pgid: pgid, createTime: confirmed})
	}
	return table, nil
}

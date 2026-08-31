//go:build darwin || linux

package runlog

import (
	"syscall"

	"github.com/shirou/gopsutil/v4/process"
)

// defaultLiveProcesses snapshots the process table once: every live
// process with the process group it belongs to and its creation time,
// the three facts the leftover reader indexes. Each row is inspected
// best-effort — a process that vanishes mid-sweep, whose group cannot
// be read, or whose creation time cannot be read is skipped, never
// reported, and the snapshot still succeeds — so one unreadable row
// can never fail the whole sweep.
func defaultLiveProcesses() ([]liveProcess, error) {
	procs, err := process.Processes()
	if err != nil {
		return nil, err
	}
	table := make([]liveProcess, 0, len(procs))
	for _, p := range procs {
		pid := int(p.Pid)
		pgid, err := syscall.Getpgid(pid)
		if err != nil {
			// The process vanished mid-sweep, or the call is not
			// permitted: without a group it cannot be attributed.
			continue
		}
		created, err := p.CreateTime()
		if err != nil {
			// A creation time that cannot be read means no identity
			// to compare: the process is skipped, never reported.
			continue
		}
		table = append(table, liveProcess{pid: pid, pgid: pgid, createTime: created})
	}
	return table, nil
}

//go:build darwin || linux

package pi

import (
	"syscall"

	"github.com/shirou/gopsutil/v4/process"
)

// descendantTarget identifies a descendant process by pid plus its process
// creation time. The creation time is the stable identity check that keeps a
// teardown sweep from killing an unrelated process that reused a dead
// descendant's pid: a target is only killed when its pid still refers to the
// same process creation observed at snapshot time.
type descendantTarget struct {
	pid        int32
	createTime int64
}

// procRow is one row of a process-table snapshot: identity plus parent pid.
// Keeping the walk over plain rows makes the traversal testable with
// synthetic tables (cycles, duplicate pids, self-parents) that the live
// table cannot produce deterministically.
type procRow struct {
	pid        int32
	ppid       int32
	createTime int64
}

// inspectDescendantTargets returns the (pid, creation-time) identities of
// every live descendant of root, captured from one process-table snapshot.
// It is a package variable so tests can force inspection failures and prove
// cleanup stays fail-safe. On any inspection error or root identity mismatch
// it returns nothing; the direct child is still terminated through its
// reaped-aware os.Process handle.
var inspectDescendantTargets = inspectDescendantTargetsImpl

func inspectDescendantTargetsImpl(root descendantTarget) []descendantTarget {
	procs, err := process.Processes()
	if err != nil {
		return nil
	}
	table := make([]procRow, 0, len(procs))
	for _, p := range procs {
		ppid, err := p.Ppid()
		if err != nil {
			continue // vanishing process; not part of the tree anymore
		}
		created, err := p.CreateTime()
		if err != nil {
			continue // unreadable identity; never kill without one
		}
		table = append(table, procRow{pid: p.Pid, ppid: ppid, createTime: created})
	}
	return buildDescendantTargets(root, table)
}

// buildDescendantTargets walks the descendant tree of root over one table
// snapshot and returns the identity of every reachable descendant. The walk
// is breadth-first over a pid->children map and visits each pid at most
// once, so corrupt rows (self-parents, cycles, duplicate pids) cannot loop
// or duplicate: the sweep is bounded by the table size. The walk descends
// only through parents present in the snapshot: a child whose parent pid is
// absent belongs to a dead-or-reused pid, not to a live lineage, and is not
// attributable to root.
func buildDescendantTargets(root descendantTarget, table []procRow) []descendantTarget {
	children := make(map[int32][]int32, len(table))
	byPID := make(map[int32]procRow, len(table))
	for _, row := range table {
		children[row.ppid] = append(children[row.ppid], row.pid)
		byPID[row.pid] = row
	}
	rootRow, ok := byPID[root.pid]
	if !ok || root.pid <= 1 || rootRow.createTime != root.createTime {
		return nil
	}
	var targets []descendantTarget
	seen := map[int32]bool{root.pid: true}
	queue := []int32{root.pid}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if _, ok := byPID[pid]; !ok {
			// Root or an intermediate parent absent from the snapshot:
			// its claimed children cannot be verified as descendants.
			continue
		}
		for _, child := range children[pid] {
			if seen[child] {
				continue
			}
			seen[child] = true
			queue = append(queue, child)
			if row, ok := byPID[child]; ok {
				targets = append(targets, descendantTarget{pid: row.pid, createTime: row.createTime})
			}
		}
	}
	return targets
}

// killDescendantTargets best-effort terminates every target whose identity
// still matches. Each kill is individually identity-verified, so a pid that
// was reused between the snapshot and the kill is left alone.
func killDescendantTargets(targets []descendantTarget) {
	for _, target := range targets {
		killDescendantTarget(target)
	}
}

// killDescendantTarget kills target only after re-verifying that its pid
// still refers to the same process creation recorded at snapshot time. A
// missing process or a mismatched creation time (pid reuse) is skipped.
func killDescendantTarget(target descendantTarget) {
	p, err := process.NewProcess(target.pid)
	if err != nil {
		return // already gone
	}
	created, err := p.CreateTime()
	if err != nil || created != target.createTime {
		return // pid reused by an unrelated process; do not kill it
	}
	_ = syscall.Kill(int(target.pid), syscall.SIGKILL)
}

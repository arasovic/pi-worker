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

// ProcessIdentity names a process by pid plus its creation time. The pair is
// the identity — a pid alone is reused — so it is what the recorder stores
// for a worker root and for each descendant it records.
type ProcessIdentity struct {
	PID        int
	CreateTime int64
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
	table, err := readProcTable()
	if err != nil {
		return nil
	}
	return buildDescendantTargets(root, table)
}

// readProcTable takes one process-table snapshot as plain rows. Rows whose
// parent pid or creation time cannot be read are skipped: a vanishing
// process is not part of the tree anymore, and an unreadable identity is
// never used to kill anything.
func readProcTable() ([]procRow, error) {
	procs, err := process.Processes()
	if err != nil {
		return nil, err
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
	return table, nil
}

// LiveDescendants returns, for each root in roots, the identities of every
// live descendant of that root, all found over one process-table snapshot.
// The result at index i belongs to roots[i]; a root with no live descendants
// yields an empty slice, and a table read error yields nil for every root. A
// root whose recorded identity does not match the process-table row — an
// exited or reused pid — yields no descendants, the same identity check the
// teardown sweep uses.
func LiveDescendants(roots []ProcessIdentity) [][]ProcessIdentity {
	table, err := readProcTable()
	if err != nil {
		return nil
	}
	return descendantsOfRoots(roots, table)
}

// descendantsOfRoots returns, for each root in roots, the identities of its
// live descendants. The process index is built once and shared across every
// root, so a sweep costs one table pass rather than one pass per root. The
// result at index i belongs to roots[i]; a root with no descendants yields a
// nil entry.
func descendantsOfRoots(roots []ProcessIdentity, table []procRow) [][]ProcessIdentity {
	idx := newProcIndex(table)
	result := make([][]ProcessIdentity, len(roots))
	for i, root := range roots {
		targets := idx.descendants(descendantTarget{pid: int32(root.PID), createTime: root.CreateTime})
		if len(targets) == 0 {
			continue
		}
		identities := make([]ProcessIdentity, len(targets))
		for j, target := range targets {
			identities[j] = ProcessIdentity{PID: int(target.pid), CreateTime: target.createTime}
		}
		result[i] = identities
	}
	return result
}

// procIndex is the child adjacency and pid->row lookup for one process-table
// snapshot, built once so many roots can be walked without re-scanning it.
type procIndex struct {
	children map[int32][]int32
	byPID    map[int32]procRow
}

// newProcIndex builds the child adjacency and pid lookup for table in one pass.
func newProcIndex(table []procRow) procIndex {
	children := make(map[int32][]int32, len(table))
	byPID := make(map[int32]procRow, len(table))
	for _, row := range table {
		children[row.ppid] = append(children[row.ppid], row.pid)
		byPID[row.pid] = row
	}
	return procIndex{children: children, byPID: byPID}
}

// descendants walks the descendant tree of root over one indexed table and
// returns the identity of every reachable descendant. The walk is
// breadth-first over a pid->children map and visits each pid at most once,
// so corrupt rows (self-parents, cycles, duplicate pids) cannot loop or
// duplicate: the sweep is bounded by the table size. The walk descends only
// through parents present in the snapshot: a child whose parent pid is
// absent belongs to a dead-or-reused pid, not to a live lineage, and is not
// attributable to root.
func (idx procIndex) descendants(root descendantTarget) []descendantTarget {
	rootRow, ok := idx.byPID[root.pid]
	if !ok || root.pid <= 1 || rootRow.createTime != root.createTime {
		return nil
	}
	var targets []descendantTarget
	seen := map[int32]bool{root.pid: true}
	queue := []int32{root.pid}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if _, ok := idx.byPID[pid]; !ok {
			// Root or an intermediate parent absent from the snapshot:
			// its claimed children cannot be verified as descendants.
			continue
		}
		for _, child := range idx.children[pid] {
			if seen[child] {
				continue
			}
			seen[child] = true
			queue = append(queue, child)
			if row, ok := idx.byPID[child]; ok {
				targets = append(targets, descendantTarget{pid: row.pid, createTime: row.createTime})
			}
		}
	}
	return targets
}

// buildDescendantTargets walks the descendant tree of a single root over one
// table snapshot. Callers sweeping many roots should build one procIndex and
// reuse it instead.
func buildDescendantTargets(root descendantTarget, table []procRow) []descendantTarget {
	return newProcIndex(table).descendants(root)
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

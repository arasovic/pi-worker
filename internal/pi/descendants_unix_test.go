//go:build darwin || linux

package pi

import (
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// spawnHold launches fakepi --hold in its own process group, so no test can
// ever signal the test runner's group, and registers cleanup that kills the
// hold if the test fails mid-way.
func spawnHold(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(fakePiBin, "--hold")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn hold: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// TestBuildDescendantTargetsWalksWholeTree is the traversal regression: the
// walk must reach descendants at any depth and every branch, carry their
// creation times, and exclude the root and unrelated processes.
func TestBuildDescendantTargetsWalksWholeTree(t *testing.T) {
	table := []procRow{
		{pid: 100, ppid: 99, createTime: 1000}, // root
		{pid: 200, ppid: 100, createTime: 2000},
		{pid: 300, ppid: 200, createTime: 3000},
		{pid: 400, ppid: 300, createTime: 4000}, // deep chain
		{pid: 250, ppid: 100, createTime: 2500}, // second branch
		{pid: 500, ppid: 99, createTime: 5000},  // unrelated
		{pid: 600, ppid: 500, createTime: 6000}, // unrelated subtree
	}
	got := buildDescendantTargets(100, table)
	want := map[int32]int64{200: 2000, 300: 3000, 400: 4000, 250: 2500}
	if len(got) != len(want) {
		t.Fatalf("targets = %+v, want %+v", got, want)
	}
	for _, target := range got {
		if want[target.pid] != target.createTime {
			t.Fatalf("target %+v not in want %+v", target, want)
		}
	}
}

// TestBuildDescendantTargetsToleratesCorruptTables is the boundedness
// regression: a self-parented root, duplicated pids, and cycles in the
// process table must terminate, visit each pid at most once, and never
// include the root. A naive walk would loop forever on this table.
func TestBuildDescendantTargetsToleratesCorruptTables(t *testing.T) {
	table := []procRow{
		{pid: 10, ppid: 10, createTime: 1}, // root is its own parent
		{pid: 11, ppid: 10, createTime: 2},
		{pid: 12, ppid: 11, createTime: 3},
		{pid: 11, ppid: 12, createTime: 4}, // duplicate pid closes a cycle
	}
	got := buildDescendantTargets(10, table)
	if len(got) != 2 {
		t.Fatalf("targets = %+v, want the two distinct descendants exactly once", got)
	}
	seen := map[int32]bool{}
	for _, target := range got {
		if target.pid == 10 || seen[target.pid] {
			t.Fatalf("target %+v duplicated or is the root", target)
		}
		seen[target.pid] = true
	}
}

// TestBuildDescendantTargetsWithMissingRoot covers the degenerate tables:
// an absent root and an empty table must both yield no targets.
func TestBuildDescendantTargetsWithMissingRoot(t *testing.T) {
	if got := buildDescendantTargets(100, []procRow{{pid: 200, ppid: 100, createTime: 2}}); len(got) != 0 {
		t.Fatalf("targets = %+v, want none when the root is absent", got)
	}
	if got := buildDescendantTargets(1, nil); len(got) != 0 {
		t.Fatalf("targets = %+v, want none for an empty table", got)
	}
}

// TestDescendantKillRejectsReusedPID is the pid-reuse protection regression:
// a target whose creation time no longer matches the snapshot identity must
// be left alone, while the exact identity must be terminated. A stale
// identity for a dead pid must be a harmless no-op.
func TestDescendantKillRejectsReusedPID(t *testing.T) {
	cmd := spawnHold(t)
	pid := int32(cmd.Process.Pid)
	item, err := process.NewProcess(pid)
	if err != nil {
		t.Fatalf("inspect hold: %v", err)
	}
	created, err := item.CreateTime()
	if err != nil {
		t.Fatalf("create time of hold: %v", err)
	}

	// A mismatched creation time (the pid was reused by another process)
	// must never be killed.
	killDescendantTarget(descendantTarget{pid: pid, createTime: created + 1})
	if !processAlive(int(pid)) {
		t.Fatalf("kill with mismatched creation time hit pid %d: pid-reuse protection failed", pid)
	}

	// The exact snapshot identity is the one legitimate kill. The hold is
	// this test's direct child, so reap it before asserting it is gone;
	// an unreaped SIGKILLed child lingers as a zombie.
	killDescendantTarget(descendantTarget{pid: pid, createTime: created})
	_ = cmd.Wait()
	waitProcessGone(t, int(pid))

	// A stale identity for the now-dead pid is a no-op, not a panic.
	killDescendantTarget(descendantTarget{pid: pid, createTime: created})
}

// TestTerminateKillsGroupWhenInspectionFails is the fail-safe regression:
// when descendant inspection errors out (or returns garbage), terminate
// must still kill the process group, must not panic, and must return the
// group-kill result rather than an inspection error.
func TestTerminateKillsGroupWhenInspectionFails(t *testing.T) {
	original := inspectDescendantTargets
	inspectDescendantTargets = func(int32) []descendantTarget {
		// Inspection failure: degrade to the group kill alone.
		return nil
	}
	t.Cleanup(func() { inspectDescendantTargets = original })

	cmd := spawnHold(t)
	cont, err := newChildContainment()
	if err != nil {
		t.Fatalf("new containment: %v", err)
	}
	if err := cont.terminate(cmd.Process.Pid); err != nil {
		t.Fatalf("terminate with failing inspection: %v", err)
	}
	_ = cmd.Wait()
	waitProcessGone(t, cmd.Process.Pid)
}

// TestTerminateIgnoresGarbageTargets covers the other fail-safe half: a
// broken inspector returning nonexistent or non-positive identities must not
// panic, must not kill anything it cannot verify, and the group kill still
// lands.
func TestTerminateIgnoresGarbageTargets(t *testing.T) {
	original := inspectDescendantTargets
	inspectDescendantTargets = func(int32) []descendantTarget {
		return []descendantTarget{
			{pid: 1, createTime: 1},       // init: never a descendant
			{pid: -5, createTime: 1},      // invalid pid
			{pid: 1 << 30, createTime: 1}, // almost certainly nonexistent
		}
	}
	t.Cleanup(func() { inspectDescendantTargets = original })

	cmd := spawnHold(t)
	cont, err := newChildContainment()
	if err != nil {
		t.Fatalf("new containment: %v", err)
	}
	if err := cont.terminate(cmd.Process.Pid); err != nil {
		t.Fatalf("terminate with garbage targets: %v", err)
	}
	_ = cmd.Wait()
	waitProcessGone(t, cmd.Process.Pid)
}

// TestTerminateRefusesNonPositivePIDWithoutInspection covers the guard that
// keeps a bad pid from signalling the caller's own group (0) or every
// process on the system (-1): terminate must refuse and must not even start
// inspection.
func TestTerminateRefusesNonPositivePIDWithoutInspection(t *testing.T) {
	called := false
	original := inspectDescendantTargets
	inspectDescendantTargets = func(int32) []descendantTarget {
		called = true
		return nil
	}
	t.Cleanup(func() { inspectDescendantTargets = original })

	cont, err := newChildContainment()
	if err != nil {
		t.Fatalf("new containment: %v", err)
	}
	for _, pid := range []int{-1, 0, 1} {
		if err := cont.terminate(pid); err == nil {
			t.Fatalf("terminate(%d) succeeded, want refusal", pid)
		}
	}
	if called {
		t.Fatalf("descendant inspection ran for a refused pid")
	}
}

// TestDescendantInspectionUnderProcessChurn is the race regression: the
// snapshot and kill paths must stay safe while processes spawn and die
// around them and while another goroutine inspects concurrently. -race
// exercises the interleavings; the assertions keep the test meaningful (a
// live descendant is always visible) without assuming a quiescent table.
func TestDescendantInspectionUnderProcessChurn(t *testing.T) {
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = inspectDescendantTargets(int32(os.Getpid()))
		}
	}()

	for i := 0; i < 8; i++ {
		cmd := spawnHold(t)
		pid := int32(cmd.Process.Pid)
		found := false
		deadline := time.Now().Add(3 * time.Second)
		for !found && time.Now().Before(deadline) {
			for _, target := range inspectDescendantTargets(int32(os.Getpid())) {
				if target.pid == pid {
					found = true
					break
				}
			}
			if !found {
				time.Sleep(5 * time.Millisecond)
			}
		}
		if !found {
			t.Fatalf("snapshot missed live descendant %d", pid)
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	close(stop)
	wg.Wait()
}

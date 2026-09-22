//go:build darwin || linux

package run

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// TestLeftoverHelperProcess is the helper entry point the leftover tests
// start as a detached child. It returns at once in the ordinary test run
// and sleeps when the child environment asks it to stay alive.
func TestLeftoverHelperProcess(t *testing.T) {
	if os.Getenv("PI_WORKER_LEFTOVER_TEST_HELPER") != "1" {
		return
	}
	time.Sleep(60 * time.Second)
}

// startLeftoverHelper starts this test binary as a detached helper in its
// own session, carrying marker in its environment when non-empty, and
// registers cleanup that kills and reaps it.
func startLeftoverHelper(t *testing.T, marker string) *exec.Cmd {
	t.Helper()
	env := append(os.Environ(), "PI_WORKER_LEFTOVER_TEST_HELPER=1")
	if marker != "" {
		env = append(env, marker)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLeftoverHelperProcess$")
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start leftover helper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

func TestFindRunProcessesReportsOnlyTheMarkedProcess(t *testing.T) {
	id := "leftover-test-" + strconv.Itoa(os.Getpid())
	since := time.Now()
	first := startLeftoverHelper(t, RunMarkerEnv+"="+id)
	startLeftoverHelper(t, RunMarkerEnv+"="+id+"x")
	startLeftoverHelper(t, "")

	var found []LeftoverProcess
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		found = findRunProcesses(id, since)
		if len(found) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	want := []LeftoverProcess{{PID: first.Process.Pid, Name: filepath.Base(os.Args[0])}}
	if !reflect.DeepEqual(found, want) {
		t.Fatalf("findRunProcesses = %+v, want %+v", found, want)
	}
}

func TestFindRunProcessesSkipsProcessesOlderThanSince(t *testing.T) {
	id := "leftover-skip-" + strconv.Itoa(os.Getpid())
	since := time.Now()
	startLeftoverHelper(t, RunMarkerEnv+"="+id)

	deadline := time.Now().Add(5 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		if len(findRunProcesses(id, since)) > 0 {
			found = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !found {
		t.Fatal("helper carrying the marker was never found")
	}

	original := processEnviron
	var calls int
	processEnviron = func(pid int32) ([]string, error) {
		calls++
		return original(pid)
	}
	t.Cleanup(func() { processEnviron = original })

	got := findRunProcesses(id, time.Now().Add(time.Hour))
	if len(got) != 0 {
		t.Fatalf("findRunProcesses = %+v, want empty", got)
	}
	if calls != 0 {
		t.Fatalf("processEnviron called %d times, want 0", calls)
	}
}

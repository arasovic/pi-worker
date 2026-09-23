//go:build darwin || linux

package run

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
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
	return startLeftoverHelperArgs(t, os.Args[0], env)
}

// startLeftoverHelperArgs starts this test binary as a detached helper
// in its own session, with argv0 as argument zero and env as its
// environment, and registers cleanup that kills and reaps it.
func startLeftoverHelperArgs(t *testing.T, argv0 string, env []string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLeftoverHelperProcess$")
	cmd.Args[0] = argv0
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

// TestProcessEnvironReadsFirstEntryWithEmptyArgv0 is a regression test
// for macOS: with an empty argv[0] the parser must not treat that empty
// chunk as padding and skip a real environment entry. The marker is the
// first environment entry, so a mis-parse hides it.
func TestProcessEnvironReadsFirstEntryWithEmptyArgv0(t *testing.T) {
	id := "leftover-argv0-" + strconv.Itoa(os.Getpid())
	marker := RunMarkerEnv + "=" + id
	env := []string{marker}
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, RunMarkerEnv+"=") {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "PI_WORKER_LEFTOVER_TEST_HELPER=1")
	cmd := startLeftoverHelperArgs(t, "", env)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := processEnviron(int32(cmd.Process.Pid))
		if err == nil {
			for _, entry := range got {
				if entry == marker {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("processEnviron never returned %q", marker)
}

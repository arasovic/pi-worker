package pi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakePiMeta struct {
	Argv []string          `json:"argv"`
	Cwd  string            `json:"cwd"`
	Env  map[string]string `json:"env"`
}

func readMeta(t *testing.T, path string) fakePiMeta {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			var meta fakePiMeta
			if err := json.Unmarshal(data, &meta); err == nil {
				return meta
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("fakepi meta file %s never became readable: %v", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestProcessStartWithExpiredDeadlineFailsBeforeSpawn(t *testing.T) {
	// An already-expired deadline must fail Start deterministically with an
	// error that wraps context.DeadlineExceeded, before any child spawns:
	// this is the start-failure path the worker classifies as timed-out.
	proc, err := NewProcess(fakePiBin, t.TempDir())
	if err != nil {
		t.Fatalf("new process: %v", err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := proc.Start(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start with expired deadline err = %v, want DeadlineExceeded", err)
	}
	if proc.started {
		t.Fatalf("process marked started although Start failed before spawn")
	}
	if err := proc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestProcessInheritsHostEnvironmentWithoutMaterializingEnv(t *testing.T) {
	// The host environment must be inherited, not read, copied, or logged.
	// A harmless test-only value proves the fake child sees the parent
	// environment, while cmd.Env must stay nil so Go performs normal
	// process environment inheritance. Environment values are never
	// printed by this test.
	t.Setenv("PI_WORKER_TEST_TOKEN", "test-only-value")
	metaPath := filepath.Join(t.TempDir(), "meta.json")
	t.Setenv("FAKEPI_META", metaPath)
	t.Setenv("FAKEPI_ENV", "PI_WORKER_TEST_TOKEN")

	workspace := t.TempDir()
	proc, err := NewProcess(fakePiBin, workspace)
	if err != nil {
		t.Fatalf("new process: %v", err)
	}
	if err := proc.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if proc.cmd.Env != nil {
		t.Fatalf("Process materialized an explicit Env slice; cmd.Env must stay nil for host environment inheritance")
	}

	meta := readMeta(t, metaPath)
	want := []string{
		fakePiBin,
		"--mode", "rpc",
		"--session-dir", proc.SessionDir(),
		"--name", processName(proc.SessionDir()),
		"--no-context-files",
		"--no-extensions",
		"--no-skills",
		"--no-prompt-templates",
		"--no-themes",
		"--no-approve",
		"--tools", "read,grep,find,ls,edit,write,bash",
	}
	if !slices.Equal(meta.Argv, want) {
		t.Fatalf("argv = %v, want %v", meta.Argv, want)
	}
	if meta.Cwd != workspace {
		resolved, err := filepath.EvalSymlinks(workspace)
		if err != nil || meta.Cwd != resolved {
			t.Fatalf("cwd = %q, want %q", meta.Cwd, workspace)
		}
	}
	if meta.Env["PI_WORKER_TEST_TOKEN"] != "test-only-value" {
		t.Fatalf("fake child did not inherit the host environment value")
	}
	if strings.Contains(strings.Join(meta.Argv, " "), "test-only-value") {
		t.Fatalf("environment value leaked into child argv: %v", meta.Argv)
	}
	if _, err := os.Stat(proc.SessionDir()); err != nil {
		t.Fatalf("session directory missing: %v", err)
	}

	if err := proc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(proc.SessionDir()); !os.IsNotExist(err) {
		t.Fatalf("session directory left behind: %v", err)
	}
}

func TestProcessCatalogProfileUsesReadOnlyTools(t *testing.T) {
	// This catches a read-only catalog process accidentally retaining the
	// coding-worker write or shell tool permissions.
	metaPath := filepath.Join(t.TempDir(), "meta.json")
	t.Setenv("FAKEPI_META", metaPath)

	proc, err := newCatalogProcess(fakePiBin, t.TempDir())
	if err != nil {
		t.Fatalf("new catalog process: %v", err)
	}
	if err := proc.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	meta := readMeta(t, metaPath)
	if !slices.Contains(meta.Argv, "read,grep,find,ls") {
		t.Fatalf("catalog argv = %v, want read-only tools", meta.Argv)
	}
	if slices.Contains(meta.Argv, toolAllowlist) {
		t.Fatalf("catalog argv retained writable worker tools: %v", meta.Argv)
	}
	if err := proc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestProcessCancellationTerminatesChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := NewProcess(fakePiBin, t.TempDir())
	if err != nil {
		t.Fatalf("new process: %v", err)
	}
	if err := proc.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	cancel()
	if err := proc.Wait(); err == nil {
		t.Fatalf("Wait after cancel returned nil, want kill error")
	}
	if err := proc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(proc.SessionDir()); !os.IsNotExist(err) {
		t.Fatalf("session directory left behind: %v", err)
	}
}

func TestProcessKillTreeSkipsAReapedChildIdentity(t *testing.T) {
	originalTerminate := terminateProcess
	called := false
	terminateProcess = func(*childContainment, int) error {
		called = true
		return nil
	}
	t.Cleanup(func() { terminateProcess = originalTerminate })

	proc := &Process{
		started: true,
		reaped:  true,
		cmd:     &exec.Cmd{Process: &os.Process{Pid: 4242}},
		cont:    &childContainment{},
	}
	proc.killTree()

	if called {
		t.Fatal("killTree reused the process-group identity after the child was reaped")
	}
}

func TestProcessCloseKillsChildThatIgnoresEOF(t *testing.T) {
	original := processCloseGrace
	processCloseGrace = 50 * time.Millisecond
	t.Cleanup(func() { processCloseGrace = original })
	t.Setenv("FAKEPI_HOLD", "1")

	proc, err := NewProcess(fakePiBin, t.TempDir())
	if err != nil {
		t.Fatalf("new process: %v", err)
	}
	if err := proc.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	started := time.Now()
	if err := proc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("close took %v, want quick kill", elapsed)
	}
	if err := proc.Wait(); err == nil {
		t.Fatalf("holding child was not killed")
	}
}

func TestProcessCloseStartedWithoutContainmentDoesNotPanic(t *testing.T) {
	proc, err := NewProcess(fakePiBin, t.TempDir())
	if err != nil {
		t.Fatalf("new process: %v", err)
	}
	// Construct the strict containment-impossible state: started but no live
	// process state and no containment resources yet still marked started.
	proc.started = true
	proc.cmd = nil
	proc.stdin = nil
	proc.stdout = nil
	proc.cont = nil
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("Process.Close panicked with started containment state: %v", recovered)
		}
	}()
	if err := proc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestProcessCloseJoinsImmediateCancelCallback is the cancellation
// synchronization regression: when the context is done immediately after
// Start, the context.AfterFunc kill callback may still be running when
// Close runs. Close must join the callback before releasing the
// containment: it must return promptly (no deadlock, no terminate/close
// race) and the child must be gone. Repeated iterations let -race exercise
// the interleavings.
func TestProcessCloseJoinsImmediateCancelCallback(t *testing.T) {
	original := processCloseGrace
	processCloseGrace = 100 * time.Millisecond
	t.Cleanup(func() { processCloseGrace = original })
	t.Setenv("FAKEPI_HOLD", "1")

	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		proc, err := NewProcess(fakePiBin, t.TempDir())
		if err != nil {
			t.Fatalf("new process: %v", err)
		}
		if err := proc.Start(ctx); err != nil {
			t.Fatalf("start: %v", err)
		}
		cancel()
		started := time.Now()
		if err := proc.Close(); err != nil {
			t.Fatalf("close after immediate cancel: %v", err)
		}
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Fatalf("close took %v, want prompt return", elapsed)
		}
		if err := proc.Wait(); err == nil {
			t.Fatalf("holding child was not killed after immediate cancel")
		}
	}
}

// openFDCount returns the number of file descriptors currently open by
// this process, for start-failure leak assertions.
func openFDCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Skipf("cannot enumerate open descriptors: %v", err)
	}
	return len(entries)
}

func TestConcurrentProcessesHaveUniqueNamesAndSessionDirs(t *testing.T) {
	// Concurrent workers in one host process must never share a session
	// directory or a --name value: the name derives from the private
	// session directory, which MkdirTemp guarantees fresh and unique per
	// worker, and exposes no secrets.
	a, err := NewProcess(fakePiBin, t.TempDir())
	if err != nil {
		t.Fatalf("new process a: %v", err)
	}
	defer a.Close()
	b, err := NewProcess(fakePiBin, t.TempDir())
	if err != nil {
		t.Fatalf("new process b: %v", err)
	}
	defer b.Close()
	c, err := NewProcess(fakePiBin, t.TempDir())
	if err != nil {
		t.Fatalf("new process c: %v", err)
	}
	defer c.Close()

	dirs := map[string]bool{}
	names := map[string]bool{}
	for _, proc := range []*Process{a, b, c} {
		if proc.SessionDir() == "" {
			t.Fatalf("empty session directory")
		}
		if dirs[proc.SessionDir()] {
			t.Fatalf("session directory %q shared by concurrent workers", proc.SessionDir())
		}
		dirs[proc.SessionDir()] = true
		if proc.name == "" {
			t.Fatalf("empty process name")
		}
		if strings.ContainsAny(proc.name, " \t\r\n/\\") {
			t.Fatalf("unsafe process name %q", proc.name)
		}
		if names[proc.name] {
			t.Fatalf("name %q shared by concurrent workers", proc.name)
		}
		names[proc.name] = true
	}
}

// TestProcessStartFailureReleasesResources is the start-failure ownership
// regression: a Start that fails after containment creation (here the
// child executable does not exist, so cmd.Start fails after both pipes
// were created) must release the containment and every created pipe, leave
// the Process unstarted, and keep Close safe. Repeated failed starts must
// not leak descriptors.
func TestProcessStartFailureReleasesResources(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	before := openFDCount(t)
	for i := 0; i < 5; i++ {
		proc, err := NewProcess(missing, t.TempDir())
		if err != nil {
			t.Fatalf("new process: %v", err)
		}
		if err := proc.Start(context.Background()); err == nil {
			t.Fatalf("Start with missing executable succeeded")
		}
		if proc.started {
			t.Fatalf("process marked started although Start failed")
		}
		if err := proc.Close(); err != nil {
			t.Fatalf("close after failed start: %v", err)
		}
	}
	after := openFDCount(t)
	if after > before+2 {
		t.Fatalf("file descriptors leaked across failed starts: before=%d after=%d", before, after)
	}
}

//go:build darwin || linux

package runlog

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// TestHelperOwnerLock is an env-gated helper that acquires the owner
// lock of the directory named by PI_WORKER_RUNLOG_TEST_OWNER_DIR in a
// real child process, writes "acquired\n" to stdout, and blocks on
// stdin until it is killed. It is never called directly by a test.
func TestHelperOwnerLock(t *testing.T) {
	dir := os.Getenv("PI_WORKER_RUNLOG_TEST_OWNER_DIR")
	if dir == "" {
		return
	}
	f, err := AcquireOwnerLock(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "acquire: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("acquired")
	buf := make([]byte, 1)
	_, _ = os.Stdin.Read(buf)
	f.Close()
	os.Exit(0)
}

func TestOwnerLockProbeSeesHeldThenFree(t *testing.T) {
	dir := t.TempDir()
	f, err := AcquireOwnerLock(dir)
	if err != nil {
		t.Fatalf("AcquireOwnerLock: %v", err)
	}
	defer f.Close()
	if got := ProbeOwnerLock(dir); got != LockHeld {
		t.Fatalf("probe while held = %v, want LockHeld", got)
	}
	f.Close()
	if got := ProbeOwnerLock(dir); got != LockFree {
		t.Fatalf("probe after close = %v, want LockFree", got)
	}
}

// TestOwnerLockReleasedWhenOwnerProcessDies asserts the lock belongs
// to the owning process: a SIGKILLed owner leaves the lock file behind,
// and the probe reads it as free.
func TestOwnerLockReleasedWhenOwnerProcessDies(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperOwnerLock$")
	cmd.Env = append(os.Environ(), "PI_WORKER_RUNLOG_TEST_OWNER_DIR="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "acquired\n" {
		t.Fatalf("helper said %q, %v; want acquired", line, err)
	}
	if got := ProbeOwnerLock(dir); got != LockHeld {
		t.Fatalf("probe while owner lives = %v, want LockHeld", got)
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	_ = cmd.Wait()
	waited = true
	if got := ProbeOwnerLock(dir); got != LockFree {
		t.Fatalf("probe after owner killed = %v, want LockFree", got)
	}
}

func TestOwnerLockProbeNeverCreatesTheFile(t *testing.T) {
	dir := t.TempDir()
	if got := ProbeOwnerLock(dir); got != LockAbsent {
		t.Fatalf("probe of empty dir = %v, want LockAbsent", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("probe left %d entries in the run directory, want none", len(entries))
	}
}

func TestOwnerLockAcquireRefusesExistingName(t *testing.T) {
	for name, plant := range map[string]func(path string) error{
		"regular file": func(path string) error { return os.WriteFile(path, nil, 0o600) },
		"symlink to file": func(path string) error {
			target := filepath.Join(filepath.Dir(path), "target")
			if err := os.WriteFile(target, nil, 0o600); err != nil {
				return err
			}
			return os.Symlink(target, path)
		},
		"dangling symlink": func(path string) error {
			return os.Symlink(filepath.Join(filepath.Dir(path), "missing"), path)
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := plant(filepath.Join(dir, OwnerLockName)); err != nil {
				t.Fatalf("plant: %v", err)
			}
			f, err := AcquireOwnerLock(dir)
			if err == nil {
				f.Close()
				t.Fatal("AcquireOwnerLock accepted a pre-existing owner.lock")
			}
			if !errors.Is(err, fs.ErrExist) {
				t.Fatalf("AcquireOwnerLock error = %v, want fs.ErrExist", err)
			}
		})
	}
}

// TestOwnerLockProbesAreShared asserts a probe takes a shared lock:
// while another probe holds its shared lock, a second probe still
// reads the unowned lock as free rather than held.
func TestOwnerLockProbesAreShared(t *testing.T) {
	dir := t.TempDir()
	f, err := AcquireOwnerLock(dir)
	if err != nil {
		t.Fatalf("AcquireOwnerLock: %v", err)
	}
	f.Close()

	other, err := os.Open(filepath.Join(dir, OwnerLockName))
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	defer other.Close()
	if err := syscall.Flock(int(other.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		t.Fatalf("hold shared lock: %v", err)
	}
	if got := ProbeOwnerLock(dir); got != LockFree {
		t.Fatalf("probe beside another probe = %v, want LockFree", got)
	}
}

func TestTryLockOwnerFailsWhileHeld(t *testing.T) {
	dir := t.TempDir()
	owner, err := AcquireOwnerLock(dir)
	if err != nil {
		t.Fatalf("AcquireOwnerLock: %v", err)
	}
	defer owner.Close()

	f, err := os.OpenFile(filepath.Join(dir, OwnerLockName), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	defer f.Close()
	if ok, err := TryLockOwner(f); ok || err != nil {
		t.Fatalf("TryLockOwner while held = %v, %v; want false, nil", ok, err)
	}
	owner.Close()
	if ok, err := TryLockOwner(f); !ok || err != nil {
		t.Fatalf("TryLockOwner after release = %v, %v; want true, nil", ok, err)
	}
}

// TestOwnerAliveLockDecidesBeforePid asserts the lock, when its file
// exists, overrules the pid: the pidAlive seam is scripted to the
// opposite answer in each case.
func TestOwnerAliveLockDecidesBeforePid(t *testing.T) {
	const pid = 4242

	held := t.TempDir()
	f, err := AcquireOwnerLock(held)
	if err != nil {
		t.Fatalf("AcquireOwnerLock: %v", err)
	}
	defer f.Close()
	withPidAlive(t, func(int32) (bool, error) { return false, nil })
	if !OwnerAlive(held, pid, 0) {
		t.Fatal("OwnerAlive with lock held and pid dead = false, want true")
	}

	free := t.TempDir()
	g, err := AcquireOwnerLock(free)
	if err != nil {
		t.Fatalf("AcquireOwnerLock: %v", err)
	}
	g.Close()
	withPidAlive(t, func(int32) (bool, error) { return true, nil })
	if OwnerAlive(free, pid, 0) {
		t.Fatal("OwnerAlive with lock free and pid alive = true, want false")
	}
}

func TestOwnerAliveWithoutLockFileUsesPidRule(t *testing.T) {
	const pid = 4242
	dir := t.TempDir()
	for _, alive := range []bool{true, false} {
		withPidAlive(t, func(int32) (bool, error) { return alive, nil })
		if got := OwnerAlive(dir, pid, 0); got != alive {
			t.Fatalf("OwnerAlive without lock file, pid alive=%v = %v", alive, got)
		}
	}
}

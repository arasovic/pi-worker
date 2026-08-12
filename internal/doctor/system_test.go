package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"pi-worker/internal/piversion"
)

func TestSystemVersionTrimsBoundedStdoutAndDiscardsStderr(t *testing.T) {
	command := writeVersionCommand(t, "0.84.1\n", "child-stderr-must-not-leak")
	version, err := systemVersion(context.Background(), command)
	if err != nil || version != "0.84.1" {
		t.Fatalf("version = %q, err = %v", version, err)
	}
}

func TestSystemVersionRejectsOversizedStdout(t *testing.T) {
	command := writeVersionCommand(t, strings.Repeat("x", piversion.MaxOutputBytes*2), "")
	if _, err := systemVersion(context.Background(), command); err == nil {
		t.Fatal("systemVersion accepted oversized stdout")
	}
}

func TestSystemVersionDoesNotHangWhenDescendantKeepsStdoutOpen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}

	pidPath := filepath.Join(t.TempDir(), "descendant.pid")
	command := writeVersionLifecycleCommand(t, pidPath)
	t.Cleanup(func() {
		pid, err := os.ReadFile(pidPath)
		if err != nil {
			return
		}
		process, err := os.FindProcess(parsePID(t, string(pid)))
		if err == nil {
			_ = process.Kill()
		}
	})
	result := make(chan error, 1)
	go func() {
		_, err := systemVersion(context.Background(), command)
		result <- err
	}()

	select {
	case err := <-result:
		if !errors.Is(err, exec.ErrWaitDelay) {
			t.Fatalf("systemVersion error = %v, want exec.ErrWaitDelay", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("systemVersion hung while descendant retained stdout")
	}
}

func writeVersionCommand(t *testing.T, stdout, stderr string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pi")
	script := "#!/bin/sh\nprintf '%s' '" + stdout + "'\nprintf '%s' '" + stderr + "' >&2\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write command: %v", err)
	}
	return path
}

func writeVersionLifecycleCommand(t *testing.T, pidPath string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pi")
	script := "#!/bin/sh\nsleep 30 &\necho $! > '" + pidPath + "'\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write command: %v", err)
	}
	return path
}

func parsePID(t *testing.T, text string) int {
	t.Helper()
	var pid int
	if _, err := fmt.Sscan(text, &pid); err != nil || pid <= 0 {
		t.Fatalf("parse descendant pid %q: %v", text, err)
	}
	return pid
}

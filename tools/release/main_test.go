package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi-worker/internal/releaseartifact"
)

func TestRunRejectsDirtyWorktree(t *testing.T) {
	temp := t.TempDir()
	oldRunCommand := runCommand
	oldVerify := verifyNotices
	oldBuild := runReleaseBuild
	runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "git" && len(args) >= 3 && args[0] == "-C" && args[2] == "status" {
			return []byte("M file"), nil
		}
		if name == "go" && len(args) > 0 && args[0] == "env" {
			return []byte(filepath.Join(temp, "modcache")), nil
		}
		return nil, nil
	}
	verifyNotices = func(_ []byte, _ string) error {
		return nil
	}
	runReleaseBuild = func(_ context.Context, _ releaseartifact.Options) error {
		t.Fatal("runReleaseBuild should not be called when worktree is dirty")
		return nil
	}
	t.Cleanup(func() {
		runCommand = oldRunCommand
		verifyNotices = oldVerify
		runReleaseBuild = oldBuild
	})

	if err := run([]string{
		"--version", "v0.1.0",
		"--commit", "0123456789abcdef0123456789abcdef01234567",
		"--build-date", "2026-08-11T00:00:00Z",
		"--output", filepath.Join(temp, "dist"),
	}); err == nil || !strings.Contains(err.Error(), "working tree has uncommitted changes") {
		t.Fatalf("run() error = %v", err)
	}
}

func TestRunBuildsWithVerifiedOptions(t *testing.T) {
	temp := t.TempDir()

	oldRunCommand := runCommand
	oldVerify := verifyNotices
	oldBuild := runReleaseBuild
	var got releaseartifact.Options

	runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "git" && len(args) >= 3 && args[0] == "-C" && args[2] == "status" {
			return nil, nil
		}
		if name == "go" && len(args) > 0 && args[0] == "env" {
			return []byte(filepath.Join(temp, "modcache")), nil
		}
		return nil, nil
	}
	verifyNotices = func(_ []byte, _ string) error { return nil }
	runReleaseBuild = func(_ context.Context, options releaseartifact.Options) error {
		got = options
		return nil
	}
	t.Cleanup(func() {
		runCommand = oldRunCommand
		verifyNotices = oldVerify
		runReleaseBuild = oldBuild
	})

	if err := run([]string{
		"--version", "v0.1.0",
		"--commit", "0123456789abcdef0123456789abcdef01234567",
		"--build-date", "2026-08-11T00:00:00Z",
		"--output", filepath.Join(temp, "dist"),
	}); err != nil {
		t.Fatalf("run() unexpected error: %v", err)
	}

	wantDate := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)
	if got.Version != "v0.1.0" || got.Commit != "0123456789abcdef0123456789abcdef01234567" || !got.BuildDate.Equal(wantDate) || got.OutputDir != filepath.Join(temp, "dist") {
		t.Fatalf("unexpected build options: %#v", got)
	}
}

func TestRunUsesRepositoryRootFromNestedWorkingDirectory(t *testing.T) {
	oldCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	repoRoot, err := filepath.Abs(filepath.Join(oldCwd, "..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root fixture: %v", err)
	}
	nested := filepath.Join(repoRoot, "internal", "releaseartifact")
	if err := os.Chdir(nested); err != nil {
		t.Fatalf("chdir nested repository directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCwd) })

	noticeData, err := os.ReadFile(filepath.Join(repoRoot, "THIRD_PARTY_NOTICES"))
	if err != nil {
		t.Fatalf("read repository notice fixture: %v", err)
	}

	oldRunCommand := runCommand
	oldVerify := verifyNotices
	oldBuild := runReleaseBuild
	var got releaseartifact.Options
	runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "git" {
			if len(args) != 4 || args[0] != "-C" || filepath.Clean(args[1]) != repoRoot || args[2] != "status" || args[3] != "--short" {
				t.Fatalf("git command = %q %v, want root-scoped status", name, args)
			}
			return nil, nil
		}
		if name == "go" && len(args) == 2 && args[0] == "env" && args[1] == "GOMODCACHE" {
			return []byte(filepath.Join(t.TempDir(), "modcache")), nil
		}
		t.Fatalf("unexpected command: %s %v", name, args)
		return nil, nil
	}
	verifyNotices = func(got []byte, _ string) error {
		if !bytes.Equal(got, noticeData) {
			t.Fatalf("notice was read from the wrong root")
		}
		return nil
	}
	runReleaseBuild = func(_ context.Context, options releaseartifact.Options) error {
		got = options
		return nil
	}
	t.Cleanup(func() {
		runCommand = oldRunCommand
		verifyNotices = oldVerify
		runReleaseBuild = oldBuild
	})

	if err := run([]string{
		"--version", "v0.1.0",
		"--commit", "0123456789abcdef0123456789abcdef01234567",
		"--build-date", "2026-08-11T00:00:00Z",
		"--output", filepath.Join(t.TempDir(), "dist"),
	}); err != nil {
		t.Fatalf("run() unexpected error: %v", err)
	}
	if got.Version != "v0.1.0" {
		t.Fatalf("release build was not invoked: %#v", got)
	}
}

func TestRunRequiresUTCBuildDate(t *testing.T) {
	if err := run([]string{
		"--version", "v0.1.0",
		"--commit", "0123456789abcdef0123456789abcdef01234567",
		"--build-date", "2026-08-11T00:00:00-07:00",
		"--output", t.TempDir(),
	}); err == nil {
		t.Fatal("expected UTC build-date validation error")
	}
}

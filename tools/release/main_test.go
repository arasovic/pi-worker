package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/releaseartifact"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

func installGitResponses(t *testing.T, status, head string) {
	t.Helper()
	oldRunGitCommand := runGitCommand
	runGitCommand = func(_ context.Context, cmd *exec.Cmd) ([]byte, error) {
		if len(cmd.Args) < 4 || cmd.Args[0] != "git" || cmd.Args[1] != "-C" {
			t.Fatalf("unexpected git command: %q", cmd.Args)
		}
		root := cmd.Args[2]
		switch {
		case reflect.DeepEqual(cmd.Args[3:], []string{"rev-parse", "--show-toplevel"}):
			return []byte(root + "\n"), nil
		case reflect.DeepEqual(cmd.Args[3:], []string{"status", "--short", "--untracked-files=all"}):
			return []byte(status), nil
		case reflect.DeepEqual(cmd.Args[3:], []string{"rev-parse", "HEAD"}):
			return []byte(head + "\n"), nil
		default:
			t.Fatalf("unexpected git command: %q", cmd.Args)
			return nil, nil
		}
	}
	t.Cleanup(func() { runGitCommand = oldRunGitCommand })
}

func TestRunGitUsesExactRootScopedStatusArguments(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	oldRunGitCommand := runGitCommand
	var got []string
	runGitCommand = func(_ context.Context, cmd *exec.Cmd) ([]byte, error) {
		got = append([]string(nil), cmd.Args...)
		return nil, nil
	}
	t.Cleanup(func() { runGitCommand = oldRunGitCommand })

	if _, err := runGit(context.Background(), root, "status", "--short", "--untracked-files=all"); err != nil {
		t.Fatalf("runGit() unexpected error: %v", err)
	}
	want := []string{"git", "-C", root, "status", "--short", "--untracked-files=all"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("git argv = %q, want %q", got, want)
	}
}

func TestSanitizedGitEnvironmentRemovesRepositoryRedirects(t *testing.T) {
	hostEnv := []string{
		"PATH=/bin",
		"GIT_DIR=/bad/repository",
		"GIT_DIR=/another/repository",
		"GIT_WORK_TREE=/bad/worktree",
		"GIT_INDEX_FILE=/bad/index",
		"GIT_OBJECT_DIRECTORY=/bad/objects",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=/bad/alternates",
		"GIT_COMMON_DIR=/bad/common",
	}
	got := sanitizedGitEnvironment(hostEnv)
	for _, key := range []string{
		"GIT_DIR",
		"GIT_WORK_TREE",
		"GIT_INDEX_FILE",
		"GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_COMMON_DIR",
	} {
		for _, entry := range got {
			if strings.HasPrefix(entry, key+"=") {
				t.Fatalf("sanitized environment retained %s: %q", key, entry)
			}
		}
	}
	if !reflect.DeepEqual(got, []string{"PATH=/bin"}) {
		t.Fatalf("sanitized environment = %q, want PATH only", got)
	}
}

func TestEnsureCleanWorktreeRejectsMismatchedGitTopLevel(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	oldRunGitCommand := runGitCommand
	calls := 0
	runGitCommand = func(_ context.Context, cmd *exec.Cmd) ([]byte, error) {
		calls++
		if reflect.DeepEqual(cmd.Args, []string{"git", "-C", root, "rev-parse", "--show-toplevel"}) {
			return []byte(filepath.Join(root, "elsewhere") + "\n"), nil
		}
		t.Fatalf("unexpected git command after top-level mismatch: %q", cmd.Args)
		return nil, nil
	}
	t.Cleanup(func() { runGitCommand = oldRunGitCommand })

	err := ensureCleanWorktree(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "repository root") {
		t.Fatalf("ensureCleanWorktree() error = %v, want repository-root mismatch", err)
	}
	if calls != 1 {
		t.Fatalf("git calls = %d, want only top-level verification", calls)
	}
}

func TestEnsureCleanWorktreeRejectsUntrackedFileStatus(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	oldRunGitCommand := runGitCommand
	runGitCommand = func(_ context.Context, cmd *exec.Cmd) ([]byte, error) {
		switch {
		case reflect.DeepEqual(cmd.Args, []string{"git", "-C", root, "rev-parse", "--show-toplevel"}):
			return []byte(root + "\n"), nil
		case reflect.DeepEqual(cmd.Args, []string{"git", "-C", root, "status", "--short", "--untracked-files=all"}):
			return []byte("?? nested/file.txt\n"), nil
		default:
			t.Fatalf("unexpected git command: %q", cmd.Args)
			return nil, nil
		}
	}
	t.Cleanup(func() { runGitCommand = oldRunGitCommand })

	err := ensureCleanWorktree(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "working tree has uncommitted changes") {
		t.Fatalf("ensureCleanWorktree() error = %v, want dirty-worktree error", err)
	}
}

func TestRunRejectsDirtyWorktree(t *testing.T) {
	temp := t.TempDir()
	installGitResponses(t, "M file", testCommit)
	oldRunCommand := runCommand
	oldVerify := verifyNotices
	oldBuild := runReleaseBuild
	runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
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
		"--commit", testCommit,
		"--build-date", "2026-08-11T00:00:00Z",
		"--output", filepath.Join(temp, "dist"),
	}); err == nil || !strings.Contains(err.Error(), "working tree has uncommitted changes") {
		t.Fatalf("run() error = %v", err)
	}
}

func TestRunBuildsWithVerifiedOptions(t *testing.T) {
	temp := t.TempDir()
	installGitResponses(t, "", testCommit)

	oldRunCommand := runCommand
	oldVerify := verifyNotices
	oldBuild := runReleaseBuild
	var got releaseartifact.Options
	var events []string

	runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "go" && len(args) == 4 && args[0] == "-C" && args[2] == "mod" && args[3] == "verify" {
			events = append(events, "modules")
			return []byte("all modules verified\n"), nil
		}
		if name == "go" && len(args) > 0 && args[0] == "env" {
			return []byte(filepath.Join(temp, "modcache")), nil
		}
		t.Fatalf("unexpected command: %s %v", name, args)
		return nil, nil
	}
	verifyNotices = func(_ []byte, _ string) error {
		events = append(events, "notices")
		return nil
	}
	runReleaseBuild = func(_ context.Context, options releaseartifact.Options) error {
		events = append(events, "build")
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
		"--commit", testCommit,
		"--build-date", "2026-08-11T00:00:00Z",
		"--output", filepath.Join(temp, "dist"),
	}); err != nil {
		t.Fatalf("run() unexpected error: %v", err)
	}

	wantDate := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)
	if got.Version != "v0.1.0" || got.Commit != testCommit || !got.BuildDate.Equal(wantDate) || got.OutputDir != filepath.Join(temp, "dist") {
		t.Fatalf("unexpected build options: %#v", got)
	}
	if !reflect.DeepEqual(events, []string{"modules", "notices", "build"}) {
		t.Fatalf("release events = %v", events)
	}
}

func TestVerifyModulesUsesRepositoryRootAndPropagatesFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	wantErr := errors.New("verification failed")
	oldRunCommand := runCommand
	runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "go" || !reflect.DeepEqual(args, []string{"-C", root, "mod", "verify"}) {
			t.Fatalf("command = %s %v", name, args)
		}
		return nil, wantErr
	}
	t.Cleanup(func() { runCommand = oldRunCommand })

	err := verifyModules(context.Background(), root)
	if !errors.Is(err, wantErr) {
		t.Fatalf("verifyModules() error = %v, want wrapped failure", err)
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
	oldRunGitCommand := runGitCommand
	oldVerify := verifyNotices
	oldBuild := runReleaseBuild
	var got releaseartifact.Options
	runGitCommand = func(_ context.Context, cmd *exec.Cmd) ([]byte, error) {
		if len(cmd.Args) < 4 || cmd.Args[0] != "git" || cmd.Args[1] != "-C" || filepath.Clean(cmd.Args[2]) != repoRoot {
			t.Fatalf("git command = %q, want repository root %q", cmd.Args, repoRoot)
		}
		switch {
		case reflect.DeepEqual(cmd.Args[3:], []string{"rev-parse", "--show-toplevel"}):
			return []byte(repoRoot + "\n"), nil
		case reflect.DeepEqual(cmd.Args[3:], []string{"status", "--short", "--untracked-files=all"}):
			return nil, nil
		case reflect.DeepEqual(cmd.Args[3:], []string{"rev-parse", "HEAD"}):
			return []byte(testCommit + "\n"), nil
		default:
			t.Fatalf("unexpected git command: %q", cmd.Args)
			return nil, nil
		}
	}
	runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "go" && len(args) == 4 && args[0] == "-C" && filepath.Clean(args[1]) == repoRoot && args[2] == "mod" && args[3] == "verify" {
			return []byte("all modules verified\n"), nil
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
		runGitCommand = oldRunGitCommand
		verifyNotices = oldVerify
		runReleaseBuild = oldBuild
	})

	if err := run([]string{
		"--version", "v0.1.0",
		"--commit", testCommit,
		"--build-date", "2026-08-11T00:00:00Z",
		"--output", filepath.Join(t.TempDir(), "dist"),
	}); err != nil {
		t.Fatalf("run() unexpected error: %v", err)
	}
	if got.Version != "v0.1.0" {
		t.Fatalf("release build was not invoked: %#v", got)
	}
}

func TestRunRejectsCommitThatDoesNotMatchHead(t *testing.T) {
	installGitResponses(t, "", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	oldBuild := runReleaseBuild
	runReleaseBuild = func(_ context.Context, _ releaseartifact.Options) error {
		t.Fatal("release build must not run for a mismatched commit")
		return nil
	}
	t.Cleanup(func() { runReleaseBuild = oldBuild })

	err := run([]string{
		"--version", "v0.1.0",
		"--commit", testCommit,
		"--build-date", "2026-08-11T00:00:00Z",
		"--output", filepath.Join(t.TempDir(), "dist"),
	})
	if err == nil || !strings.Contains(err.Error(), "does not match repository HEAD") {
		t.Fatalf("run() error = %v, want commit mismatch", err)
	}
}

func TestRunRequiresUTCBuildDate(t *testing.T) {
	if err := run([]string{
		"--version", "v0.1.0",
		"--commit", testCommit,
		"--build-date", "2026-08-11T00:00:00-07:00",
		"--output", t.TempDir(),
	}); err == nil {
		t.Fatal("expected UTC build-date validation error")
	}
}

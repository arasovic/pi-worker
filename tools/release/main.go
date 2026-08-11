package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"pi-worker/internal/releaseartifact"
	"pi-worker/internal/releasenotice"
)

var runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	return output, err
}

var runGitCommand = func(_ context.Context, cmd *exec.Cmd) ([]byte, error) {
	output, err := cmd.CombinedOutput()
	return output, err
}

var runReleaseBuild = releaseartifact.Build
var verifyNotices = releasenotice.Verify

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	version := fs.String("version", "", "release version")
	commit := fs.String("commit", "", "release commit")
	buildDate := fs.String("build-date", "", "release build date")
	output := fs.String("output", "dist", "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: release --version <value> --commit <value> --build-date <RFC3339> --output <directory>")
	}
	if *version == "" {
		return fmt.Errorf("--version is required")
	}
	if *commit == "" {
		return fmt.Errorf("--commit is required")
	}
	if *buildDate == "" {
		return fmt.Errorf("--build-date is required")
	}
	if *output == "" {
		return fmt.Errorf("--output is required")
	}

	parsedBuildDate, err := time.Parse(time.RFC3339, *buildDate)
	if err != nil {
		return fmt.Errorf("--build-date: %w", err)
	}
	if parsedBuildDate.Location() != time.UTC {
		return fmt.Errorf("--build-date must be UTC")
	}

	root, err := releaseartifact.RepositoryRoot()
	if err != nil {
		return err
	}
	if err := ensureCleanWorktree(context.Background(), root); err != nil {
		return err
	}
	head, err := resolveRepositoryHead(context.Background(), root)
	if err != nil {
		return err
	}
	if head != *commit {
		return fmt.Errorf("release commit %q does not match repository HEAD %q", *commit, head)
	}

	moduleCache, err := queryGomodcache(context.Background())
	if err != nil {
		return err
	}
	noticeData, err := os.ReadFile(filepath.Join(root, "THIRD_PARTY_NOTICES"))
	if err != nil {
		return err
	}
	if err := verifyNotices(noticeData, moduleCache); err != nil {
		return err
	}

	if err := runReleaseBuild(context.Background(), releaseartifact.Options{
		Version:   *version,
		Commit:    *commit,
		BuildDate: parsedBuildDate,
		OutputDir: *output,
	}); err != nil {
		return err
	}
	return nil
}

var gitRepositoryRedirectingEnvironment = map[string]struct{}{
	"GIT_DIR":                          {},
	"GIT_WORK_TREE":                    {},
	"GIT_INDEX_FILE":                   {},
	"GIT_OBJECT_DIRECTORY":             {},
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": {},
	"GIT_COMMON_DIR":                   {},
}

func runGit(ctx context.Context, root string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	cmd.Env = sanitizedGitEnvironment(os.Environ())
	return runGitCommand(ctx, cmd)
}

func sanitizedGitEnvironment(env []string) []string {
	sanitized := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, hasValue := strings.Cut(entry, "=")
		if hasValue {
			if _, ok := gitRepositoryRedirectingEnvironment[key]; ok {
				continue
			}
		}
		sanitized = append(sanitized, entry)
	}
	return sanitized
}

func ensureCleanWorktree(ctx context.Context, root string) error {
	topLevel, err := runGit(ctx, root, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("verify git repository root: %w", err)
	}
	reportedRoot := filepath.Clean(strings.TrimSpace(string(topLevel)))
	if reportedRoot != filepath.Clean(root) {
		return fmt.Errorf("git repository root %q does not match discovered root %q", reportedRoot, root)
	}

	status, err := runGit(ctx, root, "status", "--short", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("check git worktree: %w", err)
	}
	if strings.TrimSpace(string(status)) != "" {
		return fmt.Errorf("working tree has uncommitted changes")
	}
	return nil
}

func resolveRepositoryHead(ctx context.Context, root string) (string, error) {
	head, err := runGit(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve git HEAD: %w", err)
	}
	return strings.TrimSpace(string(head)), nil
}

func queryGomodcache(ctx context.Context) (string, error) {
	cacheBytes, err := runCommand(ctx, "go", "env", "GOMODCACHE")
	if err != nil {
		return "", fmt.Errorf("query GOMODCACHE: %w", err)
	}
	cache := strings.TrimSpace(string(cacheBytes))
	if cache == "" {
		return "", fmt.Errorf("GOMODCACHE is empty")
	}
	return cache, nil
}

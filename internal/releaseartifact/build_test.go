package releaseartifact

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestBuildRejectsInvalidVersion(t *testing.T) {
	for _, version := range []string{"", "1.0.0", "v1", "v1.0", "v1.2.3.4"} {
		t.Run(version, func(t *testing.T) {
			installNoopRunner(t)
			err := Build(context.Background(), Options{
				Version:   version,
				Commit:    "0123456789abcdef0123456789abcdef01234567",
				BuildDate: time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
				OutputDir: filepath.Join(t.TempDir(), "dist"),
			})
			if err == nil {
				t.Fatalf("Build() error = nil")
			}
		})
	}
}

func TestValidateOptionsAcceptsAndRejectsVPrefixedSemVer(t *testing.T) {
	tests := []struct {
		version string
		valid   bool
	}{
		{version: "v0.0.0", valid: true},
		{version: "v1.2.3", valid: true},
		{version: "v1.2.3-alpha", valid: true},
		{version: "v1.2.3-alpha.1", valid: true},
		{version: "v1.2.3-0", valid: true},
		{version: "v1.2.3-x.7.z", valid: true},
		{version: "v1.2.3+build", valid: true},
		{version: "v1.2.3+build.01", valid: true},
		{version: "v1.2.3-alpha+001", valid: true},
		{version: "v1.2.3-0A+build-9", valid: true},
		{version: "v01.2.3", valid: false},
		{version: "v1.02.3", valid: false},
		{version: "v1.2.03", valid: false},
		{version: "v1.2.3-01", valid: false},
		{version: "v1.2.3-", valid: false},
		{version: "v1.2.3+", valid: false},
		{version: "v1.2.3-alpha..1", valid: false},
		{version: "v1.2.3+build..1", valid: false},
		{version: "v1.2.3_alpha", valid: false},
		{version: "v1.2.3-alpha_1", valid: false},
		{version: "v1.2.3-alpha+build+more", valid: false},
		{version: "v1.2.3-α", valid: false},
	}

	for _, test := range tests {
		t.Run(test.version, func(t *testing.T) {
			err := validateOptions(Options{
				Version:   test.version,
				Commit:    "0123456789abcdef0123456789abcdef01234567",
				BuildDate: time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
				OutputDir: filepath.Join(t.TempDir(), "dist"),
			})
			if test.valid && err != nil {
				t.Fatalf("validateOptions() error = %v for valid version", err)
			}
			if !test.valid && err == nil {
				t.Fatalf("validateOptions() accepted invalid version")
			}
		})
	}
}

func TestBuildRejectsInvalidCommit(t *testing.T) {
	for _, commit := range []string{"", "012", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", "0123456789ABCDEF0123456789ABCDEF01234567"} {
		t.Run(commit, func(t *testing.T) {
			installNoopRunner(t)
			err := Build(context.Background(), Options{
				Version:   "v0.1.0",
				Commit:    commit,
				BuildDate: time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
				OutputDir: filepath.Join(t.TempDir(), "dist"),
			})
			if err == nil {
				t.Fatalf("Build() error = nil")
			}
		})
	}
}

func TestBuildRejectsNonUTCBuildDate(t *testing.T) {
	t.Run("non-utc", func(t *testing.T) {
		installNoopRunner(t)
		err := Build(context.Background(), Options{
			Version:   "v0.1.0",
			Commit:    "0123456789abcdef0123456789abcdef01234567",
			BuildDate: time.Now(),
			OutputDir: filepath.Join(t.TempDir(), "dist"),
		})
		if err == nil {
			t.Fatalf("Build() error = nil")
		}
	})
}

func TestBuildRejectsPreExistingOutputDir(t *testing.T) {
	installNoopRunner(t)

	base := t.TempDir()
	outputDir := filepath.Join(base, "dist")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatalf("create preexisting output dir: %v", err)
	}
	err := Build(context.Background(), Options{
		Version:   "v0.1.0",
		Commit:    "0123456789abcdef0123456789abcdef01234567",
		BuildDate: time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
		OutputDir: outputDir,
	})
	if err == nil {
		t.Fatalf("Build() error = nil")
	}
}

func TestBuildRejectsDuplicateTargets(t *testing.T) {
	installNoopRunner(t)

	original := append([]Target(nil), Targets...)
	Targets = []Target{{GOOS: "linux", GOARCH: "amd64"}, {GOOS: "linux", GOARCH: "amd64"}}
	t.Cleanup(func() { Targets = original })

	err := Build(context.Background(), Options{
		Version:   "v0.1.0",
		Commit:    "0123456789abcdef0123456789abcdef01234567",
		BuildDate: time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
		OutputDir: filepath.Join(t.TempDir(), "dist"),
	})
	if err == nil {
		t.Fatalf("Build() error = nil")
	}
}

func TestBuildRejectsUnsupportedTarget(t *testing.T) {
	installNoopRunner(t)

	original := append([]Target(nil), Targets...)
	Targets = []Target{{GOOS: "windows", GOARCH: "amd64"}}
	t.Cleanup(func() { Targets = original })

	err := Build(context.Background(), Options{
		Version:   "v0.1.0",
		Commit:    "0123456789abcdef0123456789abcdef01234567",
		BuildDate: time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
		OutputDir: filepath.Join(t.TempDir(), "dist"),
	})
	if err == nil {
		t.Fatalf("Build() error = nil")
	}
}

func TestBuildRejectsMissingTarget(t *testing.T) {
	originalTargets := append([]Target(nil), Targets...)
	Targets = []Target{
		{GOOS: "darwin", GOARCH: "arm64"},
		{GOOS: "darwin", GOARCH: "amd64"},
		{GOOS: "linux", GOARCH: "arm64"},
	}
	t.Cleanup(func() { Targets = originalTargets })

	oldRunner := runCommand
	runCommand = func(context.Context, *exec.Cmd) error {
		return fmt.Errorf("build command should not run")
	}
	t.Cleanup(func() { runCommand = oldRunner })

	err := Build(context.Background(), Options{
		Version:   "v0.1.0",
		Commit:    "0123456789abcdef0123456789abcdef01234567",
		BuildDate: time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
		OutputDir: filepath.Join(t.TempDir(), "dist"),
	})
	if err == nil || !strings.Contains(err.Error(), "exactly four targets") {
		t.Fatalf("Build() error = %v, want exact-target validation error", err)
	}
}

func TestBuildRemovesStagingAfterTargetFailure(t *testing.T) {
	base := t.TempDir()
	options := Options{
		Version:   "v0.1.0",
		Commit:    "0123456789abcdef0123456789abcdef01234567",
		BuildDate: time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
		OutputDir: filepath.Join(base, "dist"),
	}

	oldRunner := runCommand
	calls := 0
	runCommand = func(_ context.Context, cmd *exec.Cmd) error {
		calls++
		outputPath := argValue(cmd.Args, cmdOutputFlag)
		if err := os.WriteFile(outputPath, []byte("fake binary"), 0o755); err != nil {
			return err
		}
		if calls == 2 {
			return fmt.Errorf("forced target failure")
		}
		return nil
	}
	t.Cleanup(func() { runCommand = oldRunner })

	err := Build(context.Background(), options)
	if err == nil {
		t.Fatal("Build() error = nil")
	}
	if calls < 2 {
		t.Fatalf("build stopped before a target completed: %d calls", calls)
	}
	if _, statErr := os.Stat(options.OutputDir); !os.IsNotExist(statErr) {
		t.Fatalf("output path still exists after failure: stat error = %v", statErr)
	}
	entries, readErr := os.ReadDir(base)
	if readErr != nil {
		t.Fatalf("read staging parent: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("staging parent has residue after failure: %v", entries)
	}
}

func TestTargetEnvironmentReplacesConflictingHostEntries(t *testing.T) {
	hostEnv := []string{
		"PATH=/bin",
		"GOPROXY=https://proxy.golang.org",
		"GOPROXY=https://host.invalid",
		"GOSUMDB=sum.golang.org",
		"GOSUMDB=host.invalid",
		"GOTOOLCHAIN=auto",
		"GOTOOLCHAIN=host-toolchain",
		"GOWORK=/host/workspace.work",
		"GOWORK=/another/workspace.work",
		"GOFLAGS=-mod=mod",
		"GOFLAGS=-x",
		"GOENV=/host/goenv",
		"GOENV=/another/goenv",
		"GOTELEMETRY=local",
		"GOTELEMETRY=on",
		"GOEXPERIMENT=arenas",
		"GOEXPERIMENT=boringcrypto",
		"GOFIPS140=v1.0.0",
		"GOFIPS140=latest",
		"GOAMD64=v3",
		"GOAMD64=v4",
		"GOARM64=v9.0",
		"GOARM64=v9.1",
		"GOOS=darwin",
		"GOOS=windows",
		"GOARCH=386",
		"GOARCH=amd64",
		"CGO_ENABLED=1",
		"CGO_ENABLED=1",
	}

	got := targetEnvironmentFrom(hostEnv, Target{GOOS: "linux", GOARCH: "arm64"})
	want := map[string]string{
		"GOPROXY":      "off",
		"GOSUMDB":      "off",
		"GOTOOLCHAIN":  "local",
		"GOWORK":       "off",
		"GOFLAGS":      "-mod=readonly",
		"GOENV":        "off",
		"GOTELEMETRY":  "off",
		"GOEXPERIMENT": "none",
		"GOFIPS140":    "off",
		"GOAMD64":      "v1",
		"GOARM64":      "v8.0",
		"GOOS":         "linux",
		"GOARCH":       "arm64",
		"CGO_ENABLED":  "0",
	}
	for key, wantValue := range want {
		var values []string
		for _, entry := range got {
			if strings.HasPrefix(entry, key+"=") {
				values = append(values, strings.TrimPrefix(entry, key+"="))
			}
		}
		if len(values) != 1 || values[0] != wantValue {
			t.Fatalf("%s entries = %#v, want exactly [%q]", key, values, wantValue)
		}
	}
}

func TestBuildInvokesGoBuildAndWritesArchives(t *testing.T) {
	options := Options{
		Version:   "v0.1.0",
		Commit:    "0123456789abcdef0123456789abcdef01234567",
		BuildDate: time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
		OutputDir: filepath.Join(t.TempDir(), "dist"),
	}

	targets := append([]Target(nil), Targets...)
	var calls []commandCall
	old := runCommand
	runCommand = func(ctx context.Context, cmd *exec.Cmd) error {
		calls = append(calls, commandCall{args: append([]string(nil), cmd.Args...), env: append([]string(nil), cmd.Env...)})
		outputPath := argValue(cmd.Args, cmdOutputFlag)
		if outputPath == "" {
			return fmt.Errorf("missing %s argument", cmdOutputFlag)
		}
		if err := os.WriteFile(outputPath, []byte("fake binary"), 0o755); err != nil {
			return err
		}
		return nil
	}
	t.Cleanup(func() { runCommand = old })

	if err := Build(context.Background(), options); err != nil {
		t.Fatalf("Build() unexpected error: %v", err)
	}

	if len(calls) != len(targets) {
		t.Fatalf("got %d build invocations, want %d", len(calls), len(targets))
	}

	seen := make(map[string]bool)
	for _, call := range calls {
		if len(call.args) < 2 || call.args[0] != "go" || call.args[1] != cmdGoBuild {
			t.Fatalf("unexpected build command args: %v", call.args)
		}
		assertCommandIncludes(t, call.args, cmdTrimPath)
		assertCommandIncludes(t, call.args, cmdBuildVCS)
		outputPath := argValue(call.args, cmdOutputFlag)
		if outputPath == "" || filepath.Base(outputPath) != "pi-worker" {
			t.Fatalf("missing or invalid output path: %q", outputPath)
		}
		ldflags := argValue(call.args, cmdLDFL)
		if ldflags == "" {
			t.Fatalf("missing %s argument", cmdLDFL)
		}
		for _, expected := range []string{
			"-s",
			"-w",
			"-X pi-worker/internal/buildinfo.Version=v0.1.0",
			"-X pi-worker/internal/buildinfo.Commit=0123456789abcdef0123456789abcdef01234567",
			"-X pi-worker/internal/buildinfo.BuildDate=2026-08-11T00:00:00Z",
		} {
			if !strings.Contains(ldflags, expected) {
				t.Fatalf("ldflags %q missing %q", ldflags, expected)
			}
		}

		goos, ok := envValue(call.env, "GOOS")
		if !ok {
			t.Fatalf("missing GOOS env")
		}
		goarch, ok := envValue(call.env, "GOARCH")
		if !ok {
			t.Fatalf("missing GOARCH env")
		}
		key := goos + "/" + goarch
		if _, ok := supportedTargets[key]; !ok {
			t.Fatalf("unsupported target in command environment: %s", key)
		}
		if got := envValueMust(call.env, "CGO_ENABLED"); got != "0" {
			t.Fatalf("CGO_ENABLED = %q, want 0", got)
		}
		if seen[key] {
			t.Fatalf("duplicate build target: %s", key)
		}
		seen[key] = true
	}

	for _, target := range targets {
		archive := filepath.Join(options.OutputDir, archiveName(options.Version, target))
		if _, err := os.Stat(archive); err != nil {
			t.Fatalf("missing archive %s: %v", archive, err)
		}
		entries := readTarEntries(t, archive)
		if got, want := len(entries), 3; got != want {
			t.Fatalf("archive %s has %d entries, want %d", archive, got, want)
		}
		gotNames := make([]string, len(entries))
		for i, entry := range entries {
			gotNames[i] = entry.Name
		}
		expected := []string{"LICENSE", "THIRD_PARTY_NOTICES", "pi-worker"}
		if !reflect.DeepEqual(gotNames, expected) {
			t.Fatalf("archive %s entries = %#v, want %#v", archive, gotNames, expected)
		}
	}

	checksumPath := filepath.Join(options.OutputDir, "checksums.txt")
	checksumLines := readLines(t, checksumPath)
	expectedArchiveNames := make([]string, 0, len(targets))
	for _, target := range targets {
		expectedArchiveNames = append(expectedArchiveNames, archiveName(options.Version, target))
	}
	sort.Strings(expectedArchiveNames)
	expectedChecksumLines := make([]string, 0, len(expectedArchiveNames))
	for _, name := range expectedArchiveNames {
		data, err := os.ReadFile(filepath.Join(options.OutputDir, name))
		if err != nil {
			t.Fatalf("read archive %s for expected checksum: %v", name, err)
		}
		sum := sha256.Sum256(data)
		expectedChecksumLines = append(expectedChecksumLines, fmt.Sprintf("%x  %s", sum, name))
	}
	if !reflect.DeepEqual(checksumLines, expectedChecksumLines) {
		t.Fatalf("checksums mismatch\n got: %#v\nwant: %#v", checksumLines, expectedChecksumLines)
	}
}

func installNoopRunner(t *testing.T) {
	t.Helper()
	old := runCommand
	runCommand = func(ctx context.Context, cmd *exec.Cmd) error {
		t.Fatal("build command should not run")
		return nil
	}
	t.Cleanup(func() { runCommand = old })
}

type commandCall struct {
	args []string
	env  []string
}

func argValue(args []string, name string) string {
	for i, arg := range args {
		if arg != name {
			continue
		}
		if i+1 < len(args) {
			return args[i+1]
		}
		return ""
	}
	return ""
}

func assertCommandIncludes(t *testing.T, args []string, want string) {
	t.Helper()
	for _, arg := range args {
		if arg == want {
			return
		}
	}
	t.Fatalf("command args %v missing %q", args, want)
}

func envValue(env []string, key string) (string, bool) {
	for _, entry := range env {
		prefix := key + "="
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
	}
	return "", false
}

func envValueMust(env []string, key string) string {
	value, _ := envValue(env, key)
	return value
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if scanner.Err() != nil {
		t.Fatalf("scan %s: %v", path, scanner.Err())
	}
	return lines
}

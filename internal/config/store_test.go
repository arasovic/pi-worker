package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// writeConfig writes content to name inside dir and returns the full path.
func writeConfig(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// assertNoTempFiles fails the test if dir contains leftover temporary files
// from a failed or interrupted Save.
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("leftover temporary file %q in %s", entry.Name(), dir)
		}
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{name: "zero value", cfg: Config{}, wantErr: true},
		{name: "empty model", cfg: Config{SchemaVersion: 1}, wantErr: false},
		{name: "empty model string", cfg: Config{SchemaVersion: 1, DefaultModel: ""}, wantErr: false},
		{name: "valid selector", cfg: Config{SchemaVersion: 1, DefaultModel: "provider/model"}, wantErr: false},
		{name: "valid selector with dashes", cfg: Config{SchemaVersion: 1, DefaultModel: "openai/gpt-4o-mini"}, wantErr: false},
		{name: "schema zero", cfg: Config{SchemaVersion: 0, DefaultModel: "provider/model"}, wantErr: true},
		{name: "schema negative", cfg: Config{SchemaVersion: -1}, wantErr: true},
		{name: "schema two", cfg: Config{SchemaVersion: 2, DefaultModel: "provider/model"}, wantErr: true},
		{name: "no slash", cfg: Config{SchemaVersion: 1, DefaultModel: "model"}, wantErr: true},
		{name: "colon without slash", cfg: Config{SchemaVersion: 1, DefaultModel: "provider:model"}, wantErr: true},
		{name: "empty provider", cfg: Config{SchemaVersion: 1, DefaultModel: "/model"}, wantErr: true},
		{name: "empty id", cfg: Config{SchemaVersion: 1, DefaultModel: "provider/"}, wantErr: true},
		{name: "routing prefix in id", cfg: Config{SchemaVersion: 1, DefaultModel: "provider/upstream/model"}, wantErr: false},
		{name: "colon in id", cfg: Config{SchemaVersion: 1, DefaultModel: "provider/model:thinking"}, wantErr: false},
		{name: "space in id", cfg: Config{SchemaVersion: 1, DefaultModel: "provider/mo del"}, wantErr: false},
		{name: "space in provider", cfg: Config{SchemaVersion: 1, DefaultModel: "my provider/model"}, wantErr: false},
		{name: "tab in id", cfg: Config{SchemaVersion: 1, DefaultModel: "provider/mo\tdel"}, wantErr: false},
		{name: "newline in id", cfg: Config{SchemaVersion: 1, DefaultModel: "provider/mo\ndel"}, wantErr: false},
		{name: "leading space", cfg: Config{SchemaVersion: 1, DefaultModel: " provider/model"}, wantErr: false},
		{name: "trailing space", cfg: Config{SchemaVersion: 1, DefaultModel: "provider/model "}, wantErr: false},
		{name: "provider with slash", cfg: Config{SchemaVersion: 1, DefaultModel: "my/provider/model"}, wantErr: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := Validate(test.cfg)
			if test.wantErr && err == nil {
				t.Fatalf("Validate(%+v) = nil, want error", test.cfg)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("Validate(%+v) = %v, want nil", test.cfg, err)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    Config
		wantErr bool
	}{
		{
			name:    "minimal",
			content: `{"schemaVersion":1}`,
			want:    Config{SchemaVersion: 1},
		},
		{
			name:    "full",
			content: `{"schemaVersion":1,"defaultModel":"openai/gpt-4o-mini"}`,
			want:    Config{SchemaVersion: 1, DefaultModel: "openai/gpt-4o-mini"},
		},
		{
			name:    "trailing newline",
			content: "{\"schemaVersion\":1}\n",
			want:    Config{SchemaVersion: 1},
		},
		{
			name:    "unknown field",
			content: `{"schemaVersion":1,"defaultModel":"","policy":{}}`,
			wantErr: true,
		},
		{
			name:    "unknown field without value",
			content: `{"schemaVersion":1,"defaultModel":""}` + `,"apiKey":"secret"}`,
			wantErr: true,
		},
		{
			name:    "trailing json",
			content: `{"schemaVersion":1}{"a":1}`,
			wantErr: true,
		},
		{
			name:    "trailing garbage",
			content: `{"schemaVersion":1}xyz`,
			wantErr: true,
		},
		{
			name:    "unknown schema",
			content: `{"schemaVersion":2,"defaultModel":""}`,
			wantErr: true,
		},
		{
			name:    "missing schema",
			content: `{"defaultModel":""}`,
			wantErr: true,
		},
		{
			name:    "string schema",
			content: `{"schemaVersion":"1"}`,
			wantErr: true,
		},
		{
			name:    "invalid selector",
			content: `{"schemaVersion":1,"defaultModel":"not-a-selector"}`,
			wantErr: true,
		},
		{
			name:    "malformed json",
			content: `{"schemaVersion":`,
			wantErr: true,
		},
		{
			name:    "empty file",
			content: ``,
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeConfig(t, t.TempDir(), "config.json", test.content)
			got, err := Load(path)
			if test.wantErr {
				if err == nil {
					t.Fatalf("Load(%q) = %+v, nil error; want error", path, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load(%q) error: %v", path, err)
			}
			if got != test.want {
				t.Fatalf("Load(%q) = %+v, want %+v", path, got, test.want)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.json")
	_, err := Load(path)
	if err == nil {
		t.Fatalf("Load(%q) = nil error, want error", path)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load(%q) error = %v, want fs.ErrNotExist", path, err)
	}
}

func TestSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	want := Config{SchemaVersion: 1, DefaultModel: "provider/model"}
	if err := Save(path, want); err != nil {
		t.Fatalf("Save(%q) error: %v", path, err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q) error: %v", path, err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	assertNoTempFiles(t, dir)
}

func TestSaveCreatesMissingParents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deep", "nested", "dirs", "config.json")
	want := Config{SchemaVersion: 1, DefaultModel: "openai/gpt-4o-mini"}
	if err := Save(path, want); err != nil {
		t.Fatalf("Save(%q) error: %v", path, err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q) error: %v", path, err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	assertNoTempFiles(t, filepath.Dir(path))
}

func TestSaveRejectsInvalidBeforeTouchingExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	good := Config{SchemaVersion: 1, DefaultModel: "provider/model"}
	if err := Save(path, good); err != nil {
		t.Fatalf("Save(%q) error: %v", path, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	bad := Config{SchemaVersion: 1, DefaultModel: "not-a-selector"}
	if err := Save(path, bad); err == nil {
		t.Fatalf("Save(%q, %+v) = nil error, want validation error", path, bad)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	if string(after) != string(before) {
		t.Fatalf("existing config changed after failed Save:\nbefore: %s\nafter:  %s", before, after)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q) after failed Save error: %v", path, err)
	}
	if got != good {
		t.Fatalf("Load(%q) after failed Save = %+v, want %+v", path, got, good)
	}
	assertNoTempFiles(t, dir)
}

func TestSaveRejectsInvalidWithoutCreatingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	bad := Config{SchemaVersion: 9, DefaultModel: ""}
	if err := Save(path, bad); err == nil {
		t.Fatalf("Save(%q, %+v) = nil error, want validation error", path, bad)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat(%q) = %v, want fs.ErrNotExist", path, err)
	}
	assertNoTempFiles(t, dir)
}

func TestSaveReplacesExistingAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	first := Config{SchemaVersion: 1, DefaultModel: "alpha/one"}
	if err := Save(path, first); err != nil {
		t.Fatalf("Save(%q) error: %v", path, err)
	}
	second := Config{SchemaVersion: 1, DefaultModel: "beta/two"}
	if err := Save(path, second); err != nil {
		t.Fatalf("Save(%q) error: %v", path, err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q) error: %v", path, err)
	}
	if got != second {
		t.Fatalf("Load(%q) = %+v, want %+v", path, got, second)
	}
	assertNoTempFiles(t, dir)
}

func TestSaveFailureCleansUpTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", path, err)
	}
	cfg := Config{SchemaVersion: 1, DefaultModel: "provider/model"}
	if err := Save(path, cfg); err == nil {
		t.Fatalf("Save(%q) over a directory = nil error, want error", path)
	}
	assertNoTempFiles(t, dir)
}

func TestSaveRefusesSymlinkedDestinationWithExistingTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks is not reliably available on Windows")
	}
	// The dotfiles arrangement from the report: config.json is a link into
	// another directory that holds the real document.
	linkDir := t.TempDir()
	target := filepath.Join(t.TempDir(), "pi-worker.json")
	before := "{\"schemaVersion\":1,\"defaultModel\":\"alpha/old\"}\n"
	if err := os.WriteFile(target, []byte(before), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", target, err)
	}
	path := filepath.Join(linkDir, "config.json")
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("Symlink(%q -> %q): %v", path, target, err)
	}
	linkBefore, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(%q): %v", path, err)
	}
	targetBefore, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat(%q): %v", target, err)
	}

	err = Save(path, Config{SchemaVersion: 1, DefaultModel: "beta/new"})
	if err == nil {
		t.Fatalf("Save(%q) over a symlink = nil error, want refusal", path)
	}
	if !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("Save(%q) error = %v, want the symbolic-link refusal", path, err)
	}

	// The link is still the same link to the same target ...
	linkAfter, err := os.Lstat(path)
	if err != nil || linkAfter.Mode()&os.ModeSymlink == 0 || !os.SameFile(linkBefore, linkAfter) {
		t.Fatalf("link changed after refused Save: err=%v mode=%v", err, linkAfter.Mode())
	}
	if got, err := os.Readlink(path); err != nil || got != target {
		t.Fatalf("link target after refused Save = %q, %v; want %q", got, err, target)
	}
	// ... and the target was never written: same file, same content.
	targetAfter, err := os.Stat(target)
	if err != nil || !os.SameFile(targetBefore, targetAfter) {
		t.Fatalf("target replaced after refused Save: err=%v", err)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != before {
		t.Fatalf("target content after refused Save = %q, %v; want %q", content, err, before)
	}
	// The refusal happened before any temporary file was created.
	assertNoTempFiles(t, linkDir)
}

func TestSaveRefusesDanglingSymlinkedDestination(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks is not reliably available on Windows")
	}
	dir := t.TempDir()
	missingTarget := filepath.Join(t.TempDir(), "not", "there", "pi-worker.json")
	path := filepath.Join(dir, "config.json")
	if err := os.Symlink(missingTarget, path); err != nil {
		t.Fatalf("Symlink(%q -> %q): %v", path, missingTarget, err)
	}

	err := Save(path, Config{SchemaVersion: 1, DefaultModel: "beta/new"})
	if err == nil {
		t.Fatalf("Save(%q) over a dangling symlink = nil error, want refusal", path)
	}
	if !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("Save(%q) error = %v, want the symbolic-link refusal", path, err)
	}

	// The dangling link is untouched: still a link, still pointing where it
	// pointed, and the target — its parent directories included — was never
	// created.
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("dangling link changed after refused Save: err=%v mode=%v", err, info.Mode())
	}
	if got, err := os.Readlink(path); err != nil || got != missingTarget {
		t.Fatalf("dangling link target after refused Save = %q, %v; want %q", got, err, missingTarget)
	}
	if _, err := os.Stat(missingTarget); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat(%q) after refused Save = %v, want fs.ErrNotExist", missingTarget, err)
	}
	assertNoTempFiles(t, dir)
}

func TestSaveRefusalLeavesLinkAndTargetUntouched(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks is not reliably available on Windows")
	}
	// Link and target share one directory, the tightest arrangement for a
	// write-through or link-replacing implementation to disturb.
	dir := t.TempDir()
	target := filepath.Join(dir, "pi-worker.json")
	before := "{\"schemaVersion\":1,\"defaultModel\":\"alpha/old\"}\n"
	if err := os.WriteFile(target, []byte(before), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", target, err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("Symlink(%q -> %q): %v", path, target, err)
	}
	targetBefore, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat(%q): %v", target, err)
	}

	if err := Save(path, Config{SchemaVersion: 1, DefaultModel: "beta/new"}); err == nil {
		t.Fatalf("Save(%q) over a symlink = nil error, want refusal", path)
	}

	// The directory holds exactly what it held before — the link and its
	// target, nothing else: no replaced link, no written-through target, no
	// stray regular config.json, no temporary file.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	if len(entries) != 2 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("directory after refused Save holds %v, want exactly config.json and pi-worker.json", names)
	}
	// The link is still the link, and the target is still the same regular
	// file with the same content and the same owner-only mode.
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link changed after refused Save: err=%v mode=%v", err, info.Mode())
	}
	targetAfter, err := os.Stat(target)
	if err != nil || !os.SameFile(targetBefore, targetAfter) {
		t.Fatalf("target replaced after refused Save: err=%v", err)
	}
	if perm := targetAfter.Mode().Perm(); perm != 0o600 {
		t.Fatalf("target mode after refused Save = %o, want 600", perm)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != before {
		t.Fatalf("target content after refused Save = %q, %v; want %q", content, err, before)
	}
}

func TestSaveThroughSymlinkedParentDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks is not reliably available on Windows")
	}
	realDir := t.TempDir()
	// The link names only the directory; the config.json inside it is a plain
	// regular file that Save creates and atomically replaces as usual.
	linkDir := filepath.Join(t.TempDir(), "linked-config-dir")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatalf("Symlink(%q -> %q): %v", linkDir, realDir, err)
	}
	path := filepath.Join(linkDir, "config.json")

	first := Config{SchemaVersion: 1, DefaultModel: "alpha/one"}
	if err := Save(path, first); err != nil {
		t.Fatalf("Save(%q) through a symlinked parent error: %v", path, err)
	}
	second := Config{SchemaVersion: 1, DefaultModel: "beta/two"}
	if err := Save(path, second); err != nil {
		t.Fatalf("Save(%q) replace through a symlinked parent error: %v", path, err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q) through a symlinked parent error: %v", path, err)
	}
	if got != second {
		t.Fatalf("Load(%q) = %+v, want %+v", path, got, second)
	}
	// The write landed in the real directory, the parent link is untouched,
	// and the file left behind is a regular owner-only file like any other
	// save.
	realPath := filepath.Join(realDir, "config.json")
	info, err := os.Lstat(realPath)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("real config after save = %v, %v; want a regular file", info, err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("real config mode %o: group and other bits must be unset", perm)
	}
	if dirInfo, err := os.Lstat(linkDir); err != nil || dirInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("parent link changed after save: %v, %v", dirInfo, err)
	}
	assertNoTempFiles(t, realDir)
}

func TestSaveSetsOwnerOnlyPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on Windows")
	}
	dir := filepath.Join(t.TempDir(), "pi-worker")
	path := filepath.Join(dir, "config.json")
	if err := Save(path, Config{SchemaVersion: 1, DefaultModel: "provider/model"}); err != nil {
		t.Fatalf("Save(%q) error: %v", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) error: %v", path, err)
	}
	perm := info.Mode().Perm()
	if perm&0o077 != 0 {
		t.Fatalf("config mode %o: group and other bits must be unset", perm)
	}
	if perm&0o400 == 0 {
		t.Fatalf("config mode %o: owner read bit must be set", perm)
	}
	if perm&0o200 == 0 {
		t.Fatalf("config mode %o: owner write bit must be set", perm)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat(%q) error: %v", dir, err)
	}
	if dirPerm := dirInfo.Mode().Perm(); dirPerm&0o077 != 0 {
		t.Fatalf("config directory mode %o: group and other bits must be unset", dirPerm)
	}
}

func isolatedUserConfigPath(t *testing.T) string {
	t.Helper()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("AppData", filepath.Join(t.TempDir(), "AppData"))
	case "darwin":
		t.Setenv("HOME", t.TempDir())
	default:
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	}
	path, err := UserPath()
	if err != nil {
		t.Fatalf("UserPath(): %v", err)
	}
	return path
}

func TestSaveTightensExistingPiWorkerDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on Windows")
	}
	path := isolatedUserConfigPath(t)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(dir), err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("Mkdir(%q): %v", dir, err)
	}
	if err := Save(path, Config{SchemaVersion: 1, DefaultModel: "provider/model"}); err != nil {
		t.Fatalf("Save(%q): %v", path, err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat(%q): %v", dir, err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("config directory mode %o: group and other bits must be unset", perm)
	}
}

func TestSaveDoesNotTightenUnrelatedPiWorkerDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on Windows")
	}
	_ = isolatedUserConfigPath(t)
	dir := filepath.Join(t.TempDir(), "pi-worker")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("Mkdir(%q): %v", dir, err)
	}
	before, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat(%q): %v", dir, err)
	}
	path := filepath.Join(dir, "config.json")
	if err := Save(path, Config{SchemaVersion: 1, DefaultModel: "provider/model"}); err != nil {
		t.Fatalf("Save(%q): %v", path, err)
	}
	after, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat(%q): %v", dir, err)
	}
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Fatalf("unrelated directory mode changed from %o to %o", before.Mode().Perm(), after.Mode().Perm())
	}
}

func TestSaveAbortsBeforeReplacingConfigWhenDirectoryChmodFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not support Unix permission bits")
	}
	path := isolatedUserConfigPath(t)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(dir), err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("Mkdir(%q): %v", dir, err)
	}
	before := []byte("{\"schemaVersion\":1,\"defaultModel\":\"provider/old\"}\n")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
	want := errors.New("chmod denied")
	original := chmodConfigDirectory
	chmodConfigDirectory = func(string, os.FileMode) error { return want }
	t.Cleanup(func() { chmodConfigDirectory = original })

	err := Save(path, Config{SchemaVersion: 1, DefaultModel: "provider/new"})
	if !errors.Is(err, want) {
		t.Fatalf("Save() error = %v, want wrapped %v", err, want)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	if string(after) != string(before) {
		t.Fatalf("config changed after chmod failure: before %q, after %q", before, after)
	}
	assertNoTempFiles(t, dir)
}

type fakeDirectorySyncHandle struct {
	syncErr  error
	closeErr error
}

func (f *fakeDirectorySyncHandle) Sync() error  { return f.syncErr }
func (f *fakeDirectorySyncHandle) Close() error { return f.closeErr }

func TestSaveReturnsDirectorySyncIOErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not sync parent directories")
	}
	for _, test := range []struct {
		name     string
		openErr  error
		syncErr  error
		closeErr error
	}{
		{name: "open", openErr: errors.New("directory open failed")},
		{name: "sync", syncErr: errors.New("directory sync failed")},
		{name: "close", closeErr: errors.New("directory close failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := openDirectoryForSync
			openDirectoryForSync = func(string) (directorySyncHandle, error) {
				if test.openErr != nil {
					return nil, test.openErr
				}
				return &fakeDirectorySyncHandle{syncErr: test.syncErr, closeErr: test.closeErr}, nil
			}
			t.Cleanup(func() { openDirectoryForSync = original })

			err := Save(filepath.Join(t.TempDir(), "config.json"), Config{SchemaVersion: 1})
			want := test.openErr
			if want == nil {
				want = test.syncErr
			}
			if want == nil {
				want = test.closeErr
			}
			if !errors.Is(err, want) {
				t.Fatalf("Save() error = %v, want wrapped %v", err, want)
			}
		})
	}
}

func TestSaveToleratesOnlyUnsupportedDirectorySync(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not sync parent directories")
	}
	original := openDirectoryForSync
	openDirectoryForSync = func(string) (directorySyncHandle, error) {
		return &fakeDirectorySyncHandle{syncErr: syscall.EINVAL}, nil
	}
	t.Cleanup(func() { openDirectoryForSync = original })

	if err := Save(filepath.Join(t.TempDir(), "config.json"), Config{SchemaVersion: 1}); err != nil {
		t.Fatalf("Save() unsupported directory sync error: %v", err)
	}
}

func TestSaveReturnsUnsupportedCodesOutsideDirectorySync(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not sync parent directories")
	}
	for _, test := range []struct {
		name     string
		openErr  error
		closeErr error
	}{
		{name: "open", openErr: syscall.EINVAL},
		{name: "close", closeErr: syscall.EINVAL},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := openDirectoryForSync
			openDirectoryForSync = func(string) (directorySyncHandle, error) {
				if test.openErr != nil {
					return nil, test.openErr
				}
				return &fakeDirectorySyncHandle{closeErr: test.closeErr}, nil
			}
			t.Cleanup(func() { openDirectoryForSync = original })

			err := Save(filepath.Join(t.TempDir(), "config.json"), Config{SchemaVersion: 1})
			want := test.openErr
			if want == nil {
				want = test.closeErr
			}
			if !errors.Is(err, want) {
				t.Fatalf("Save() error = %v, want wrapped %v", err, want)
			}
		})
	}
}

func TestUserPath(t *testing.T) {
	switch runtime.GOOS {
	case "windows":
		t.Setenv("AppData", filepath.Join(t.TempDir(), "AppData"))
	case "darwin":
		t.Setenv("HOME", t.TempDir())
	default:
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	}
	path, err := UserPath()
	if err != nil {
		t.Fatalf("UserPath() error: %v", err)
	}
	if path == "" {
		t.Fatal("UserPath() returned an empty path")
	}
	wantSuffix := filepath.Join("pi-worker", "config.json")
	if !strings.HasSuffix(filepath.FromSlash(path), filepath.FromSlash(wantSuffix)) {
		t.Fatalf("UserPath() = %q, want suffix %q", path, wantSuffix)
	}
}

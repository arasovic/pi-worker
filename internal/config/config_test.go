package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestConfigEmptyDefaultIsProviderNeutral(t *testing.T) {
	cfg := Config{SchemaVersion: 1}
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate(%+v) error: %v", cfg, err)
	}
}

func TestUserDir(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-specific configuration-directory behavior")
	}
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", home, err)
	}
	if err := os.WriteFile(filepath.Join(home, "witness"), []byte("home"), 0o600); err != nil {
		t.Fatalf("prime home write: %v", err)
	}
	t.Setenv("HOME", home)

	for _, tc := range []struct {
		name string
		xdg  string
	}{
		{name: "xdg unset", xdg: ""},
		{name: "xdg absolute", xdg: filepath.Join(t.TempDir(), "abs-config")},
		{name: "xdg relative", xdg: "relative-config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.xdg == "" {
				t.Setenv("XDG_CONFIG_HOME", "")
			} else {
				t.Setenv("XDG_CONFIG_HOME", tc.xdg)
			}

			got, err := UserDir()
			if err != nil {
				t.Fatalf("UserDir(): %v", err)
			}
			root, err := os.UserConfigDir()
			if err != nil {
				t.Fatalf("os.UserConfigDir(): %v", err)
			}
			want := filepath.Join(root, "pi-worker")
			if got != want {
				t.Fatalf("UserDir() = %q, want %q", got, want)
			}
		})
	}
}

func TestUserDirMacPathSemantics(t *testing.T) {
	macUser := "/" + "Users" + "/example"
	got := configDirForGoos("darwin", macUser, "/unused")
	if got != filepath.Join(macUser, "Library", "Application Support") {
		t.Fatalf("configDirForGoos(darwin, ...): %q, want %q", got, filepath.Join(macUser, "Library", "Application Support"))
	}
}

func TestUserPathRelativeToUserDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir all: %v", err)
	}
	if runtime.GOOS == "windows" {
		t.Setenv("AppData", dir)
	} else {
		t.Setenv("HOME", dir)
	}
	userDir, err := UserDir()
	if err != nil {
		t.Fatalf("UserDir(): %v", err)
	}
	got, err := UserPath()
	if err != nil {
		t.Fatalf("UserPath(): %v", err)
	}
	if filepath.Dir(got) != filepath.Clean(userDir) {
		t.Fatalf("UserPath() directory = %q, want %q", filepath.Dir(got), filepath.Clean(userDir))
	}
	if filepath.Base(got) != "config.json" {
		t.Fatalf("UserPath() file = %q, want %q", filepath.Base(got), "config.json")
	}
	if !strings.HasSuffix(filepath.Clean(got), filepath.Join("pi-worker", "config.json")) {
		t.Fatalf("UserPath() %q does not end with pi-worker/config.json", got)
	}
}

func configDirForGoos(goos, home, xdgConfigHome string) string {
	if goos == "darwin" {
		return filepath.Join(home, "Library", "Application Support")
	}
	if xdgConfigHome != "" {
		return xdgConfigHome
	}
	return filepath.Join(home, ".config")
}

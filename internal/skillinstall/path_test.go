package skillinstall

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"pi-worker/internal/config"
)

func TestUserReceiptPath(t *testing.T) {
	root := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("AppData", filepath.Join(root, "AppData"))
	} else {
		home := filepath.Join(root, "home")
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatalf("MkdirAll(%q): %v", home, err)
		}
		t.Setenv("HOME", home)
		if runtime.GOOS == "linux" {
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
		}
	}

	got, err := UserReceiptPath()
	if err != nil {
		t.Fatalf("UserReceiptPath(): %v", err)
	}
	if filepath.Base(got) != "skill-install.json" {
		t.Fatalf("UserReceiptPath() file = %q, want %q", filepath.Base(got), "skill-install.json")
	}
	userDir, err := config.UserDir()
	if err != nil {
		t.Fatalf("config.UserDir(): %v", err)
	}
	if filepath.Dir(got) != filepath.Clean(userDir) {
		t.Fatalf("UserReceiptPath() = %q, expected to live in %q", got, userDir)
	}
	if !strings.HasSuffix(filepath.Clean(got), filepath.Join("pi-worker", "skill-install.json")) {
		t.Fatalf("UserReceiptPath() = %q, want sibling under pi-worker", got)
	}
}

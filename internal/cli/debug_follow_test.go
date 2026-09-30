package cli

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer is a strings.Builder safe to read while the follower writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestFollowDebugLogCopiesWholeLinesOnly requires that the follower waits
// for a file that does not exist yet, streams lines while it follows, never
// copies half a line, and copies what is left when stopped.
func TestFollowDebugLogCopiesWholeLinesOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "debug.log")
	var out lockedBuffer
	stop := followDebugLog(path, &out)

	// The file appears only after following began.
	time.Sleep(2 * debugFollowInterval)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("create debug file: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString("[pi-worker +0s] worker=1 one\n[pi-worker +1s] worker=1 tw"); err != nil {
		t.Fatalf("write: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for out.String() == "" && time.Now().Before(deadline) {
		time.Sleep(debugFollowInterval)
	}
	if got := out.String(); got != "[pi-worker +0s] worker=1 one\n" {
		t.Fatalf("followed = %q, want only the first whole line", got)
	}

	if _, err := f.WriteString("o\n[pi-worker +2s] worker=2 three\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	stop()
	want := "[pi-worker +0s] worker=1 one\n[pi-worker +1s] worker=1 two\n[pi-worker +2s] worker=2 three\n"
	if got := out.String(); got != want {
		t.Fatalf("after stop = %q, want %q", got, want)
	}
}

// TestFollowDebugLogWithoutAFileWritesNothing requires that a run with no
// debug file is followed without error and without output.
func TestFollowDebugLogWithoutAFileWritesNothing(t *testing.T) {
	var out lockedBuffer
	followDebugLog(filepath.Join(t.TempDir(), "debug.log"), &out)()
	if got := out.String(); got != "" {
		t.Fatalf("followed = %q, want nothing", got)
	}
}

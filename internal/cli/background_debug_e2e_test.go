package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBackgroundDebugRunIsStreamedByRunsWaitDebug requires that --debug on a
// background run is not dropped: the start names the run's debug file on
// stderr and leaves stdout one document, the workers write their lines to
// that private file, `runs wait --debug` puts them on stderr, and a
// `runs wait` without --debug prints none of them.
func TestBackgroundDebugRunIsStreamedByRunsWaitDebug(t *testing.T) {
	manager, root := setupBackgroundRun(t, slowBackgroundScript("debug answer", time.Second))

	code, stdout, stderr := runCLI(t, []string{"run", "--background", "--json", "--debug", "--model", "acme/m-1", "--task", "go", "--timeout", "5m"}, "")
	if code != 0 {
		t.Fatalf("run --background --debug = (%d, %q, %q), want 0", code, stdout, stderr)
	}
	runID, ok := decodeJSONObject(t, stdout)["runId"].(string)
	if !ok || runID == "" {
		t.Fatalf("accepted run reported no identity: %q", stdout)
	}
	t.Cleanup(func() {
		if _, err := manager.Wait(context.Background(), runID, 90*time.Second); err != nil {
			t.Errorf("drain run %s: %v", runID, err)
		}
	})
	if strings.Count(stdout, "\n") != 1 {
		t.Fatalf("stdout = %q, want exactly one JSON document", stdout)
	}
	debugLog := filepath.Join(root, runID, "debug.log")
	if !strings.Contains(stderr, "pi-worker: debug log "+debugLog+"\n") {
		t.Fatalf("stderr = %q, want the debug file %s named", stderr, debugLog)
	}

	code, stdout, stderr = runCLI(t, []string{"runs", "wait", runID, "--debug", "--timeout", "90s"}, "")
	if code != 0 {
		t.Fatalf("runs wait --debug = (%d, %q, %q), want 0", code, stdout, stderr)
	}
	for _, want := range []string{"worker=1 phase=starting", "worker=1 status=completed"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("runs wait --debug stderr = %q, want a %q line", stderr, want)
		}
	}
	for _, line := range strings.Split(strings.TrimSuffix(stderr, "\n"), "\n") {
		if !strings.HasPrefix(line, "[pi-worker +") {
			t.Fatalf("runs wait --debug stderr line %q is not a whole debug line", line)
		}
	}
	if strings.Contains(stdout, "[pi-worker +") || !strings.Contains(stdout, "outcome=completed") {
		t.Fatalf("runs wait --debug stdout = %q, want the finished run and no debug line", stdout)
	}

	info, err := os.Stat(debugLog)
	if err != nil {
		t.Fatalf("stat debug file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("debug file mode = %v, want 0600", perm)
	}

	code, stdout, stderr = runCLI(t, []string{"runs", "wait", runID}, "")
	if code != 0 || strings.Contains(stderr, "[pi-worker +") || strings.Contains(stdout, "[pi-worker +") {
		t.Fatalf("runs wait without --debug = (%d, %q, %q), want exit 0 and no debug line", code, stdout, stderr)
	}
}

// TestRunsDebugIsOnlyValidWithWait keeps --debug a wait flag: the other runs
// commands have no run to follow and refuse it as a usage error.
func TestRunsDebugIsOnlyValidWithWait(t *testing.T) {
	if _, err := parseRunsArgs([]string{"wait", "20260830T101500Z-4242", "--debug"}); err != nil {
		t.Fatalf("runs wait --debug: %v", err)
	}
	for _, args := range [][]string{
		{"list", "--debug"},
		{"prune", "--keep", "1", "--debug"},
		{"status", "20260830T101500Z-4242", "--debug"},
		{"cancel", "20260830T101500Z-4242", "--debug"},
		{"wait", "20260830T101500Z-4242", "--debug=yes"},
		{"wait", "20260830T101500Z-4242", "--debug", "--debug"},
	} {
		if _, err := parseRunsArgs(args); err == nil {
			t.Errorf("runs %v parsed, want a usage error", args)
		}
	}
}

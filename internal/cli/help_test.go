package cli

import (
	"context"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/arasovic/pi-worker/internal/pi"
)

// TestRunHelp checks that `run --help` and `run -h` print the detailed run
// help on stdout and exit 0, through both top-level dispatch seams: Main
// (with its signal handling) and mainWithContext (the deterministic seam).
func TestRunHelp(t *testing.T) {
	callers := []struct {
		name string
		call func(t *testing.T, args []string) (int, string, string)
	}{
		{"Main", func(t *testing.T, args []string) (int, string, string) {
			return runCLI(t, args, "")
		}},
		{"mainWithContext", func(t *testing.T, args []string) (int, string, string) {
			return runCLIWithContext(t, context.Background(), args, "")
		}},
	}
	for _, caller := range callers {
		for _, flag := range []string{"--help", "-h"} {
			t.Run(caller.name+"/"+flag, func(t *testing.T) {
				code, stdout, stderr := caller.call(t, []string{"run", flag})
				if code != 0 {
					t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
				}
				if stderr != "" {
					t.Fatalf("stderr = %q, want empty", stderr)
				}
				const wantPrefix = "pi-worker run - hand tasks to Pi workers"
				if !strings.HasPrefix(stdout, wantPrefix) {
					t.Fatalf("stdout = %q, want prefix %q", stdout, wantPrefix)
				}
				const wantSection = "Outcome and exit code:"
				if !strings.Contains(stdout, wantSection) {
					t.Fatalf("stdout = %q, want it to contain %q", stdout, wantSection)
				}
			})
		}
	}
}

// TestRunTaskHelpIsNotHelpRequest pins that only the first argument after the
// command is a help request: a --help consumed as the value of --task is a
// task prompt, not a request for help.
func TestRunTaskHelpIsNotHelpRequest(t *testing.T) {
	newGitWorkspace(t)
	installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "done"})
	_, stdout, _ := runCLI(t, []string{"run", "--task", "--help", "--model", "acme/m-1"}, "")
	const wantSection = "Outcome and exit code:"
	if strings.Contains(stdout, wantSection) {
		t.Fatalf("stdout = %q, want no %q", stdout, wantSection)
	}
}

// TestRunHelpLinesFit checks that the run help stays readable at 80 columns.
func TestRunHelpLinesFit(t *testing.T) {
	for i, line := range strings.Split(commandHelp["run"], "\n") {
		if len(line) > 80 {
			t.Errorf("commandHelp[run] line %d is %d bytes, want at most 80: %q", i+1, len(line), line)
		}
	}
}

// TestRunHelpCoversEveryRunFlag keeps the run help from going stale: every
// run flag literal in main.go must appear in commandHelp[run], and the set of
// such flags must match the literal list below. A new flag fails this test
// until the list and the help are both updated.
func TestRunHelpCoversEveryRunFlag(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	flagLiteral := regexp.MustCompile(`"--[a-z][a-z-]*"`)
	got := make(map[string]bool)
	for _, match := range flagLiteral.FindAllString(string(source), -1) {
		flag := strings.Trim(match, `"`)
		// --help and --version are top-level dispatch tokens rather than
		// run flags, so they are not part of the run help surface.
		if flag == "--help" || flag == "--version" {
			continue
		}
		got[flag] = true
	}
	want := map[string]bool{
		"--background": true,
		"--data":       true,
		"--debug":      true,
		"--json":       true,
		"--model":      true,
		"--task":       true,
		"--task-file":  true,
		"--thinking":   true,
		"--timeout":    true,
		"--verify":     true,
		"--worktree":   true,
		"--writes":     true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("run flags in main.go = %v, want %v", got, want)
	}
	for flag := range want {
		if !strings.Contains(commandHelp["run"], flag) {
			t.Errorf("commandHelp[run] is missing run flag %s", flag)
		}
	}
}

// TestTopLevelHelpMentionsCommandHelp checks that the top-level usage points
// at `pi-worker <command> --help`.
func TestTopLevelHelpMentionsCommandHelp(t *testing.T) {
	code, stdout, stderr := runCLI(t, []string{"--help"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	const want = "Details for one command: pi-worker <command> --help"
	if !strings.Contains(stdout, want) {
		t.Fatalf("stdout = %q, want it to contain %q", stdout, want)
	}
}

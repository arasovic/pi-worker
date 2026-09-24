package cli

import (
	"context"
	"os"
	"reflect"
	"regexp"
	"sort"
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

// TestCommandHelp checks that every top-level command answers --help and -h
// with its detailed help on stdout and exit 0, through both top-level
// dispatch seams: Main (with its signal handling) and mainWithContext (the
// deterministic seam). A help request placed after a subcommand works too.
func TestCommandHelp(t *testing.T) {
	cases := []struct {
		args     []string
		wantLine string
	}{
		{[]string{"models", "--help"}, "pi-worker models - list the model selectors Pi reports"},
		{[]string{"doctor", "-h"}, "pi-worker doctor - check that this machine is ready"},
		{[]string{"config", "--help"}, "pi-worker config - show or set the personal defaults"},
		{[]string{"config", "set", "--help"}, "pi-worker config - show or set the personal defaults"},
		{[]string{"skill", "--help"}, "pi-worker skill - report on the installed agent skill"},
		{[]string{"skill", "status", "-h"}, "pi-worker skill - report on the installed agent skill"},
		{[]string{"runs", "--help"}, "pi-worker runs - list, follow and clean up runs"},
		{[]string{"runs", "wait", "--help"}, "pi-worker runs - list, follow and clean up runs"},
		{[]string{"worktrees", "remove", "--help"}, "pi-worker worktrees - list and remove the checkouts"},
		{[]string{"version", "--help"}, "pi-worker version - print which build this is"},
		{[]string{"--version", "--help"}, "pi-worker version - print which build this is"},
	}
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
		for _, tc := range cases {
			t.Run(caller.name+"/"+strings.Join(tc.args, " "), func(t *testing.T) {
				code, stdout, stderr := caller.call(t, tc.args)
				if code != 0 {
					t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
				}
				if stderr != "" {
					t.Fatalf("stderr = %q, want empty", stderr)
				}
				if !strings.HasPrefix(stdout, tc.wantLine) {
					t.Fatalf("stdout = %q, want prefix %q", stdout, tc.wantLine)
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

// TestCommandHelpLinesFit checks that every command help stays readable at
// 80 columns.
func TestCommandHelpLinesFit(t *testing.T) {
	for command, text := range commandHelp {
		for i, line := range strings.Split(text, "\n") {
			if len(line) > 80 {
				t.Errorf("commandHelp[%s] line %d is %d bytes, want at most 80: %q", command, i+1, len(line), line)
			}
		}
	}
}

// TestCommandHelpKeys pins the command set, so a new command must add help
// before it can answer --help.
func TestCommandHelpKeys(t *testing.T) {
	want := []string{"config", "doctor", "models", "run", "runs", "skill", "version", "worktrees"}
	got := make([]string, 0, len(commandHelp))
	for key := range commandHelp {
		got = append(got, key)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("commandHelp keys = %v, want %v", got, want)
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

// TestCommandHelpCoversEveryFlag keeps each command's help from going stale:
// every flag literal in a command's source files must appear in its
// commandHelp text, and the set of such flags must match the literal list
// below. A new flag fails this test until the list and the help are both
// updated.
func TestCommandHelpCoversEveryFlag(t *testing.T) {
	cases := []struct {
		help    string
		sources []string
		want    map[string]bool
	}{
		{"config", []string{"config.go"}, map[string]bool{"--debug": true, "--json": true, "--timeout": true}},
		{"doctor", []string{"doctor.go"}, map[string]bool{"--debug": true, "--json": true, "--timeout": true}},
		{"models", []string{"models.go"}, map[string]bool{"--debug": true, "--json": true, "--timeout": true}},
		{"runs", []string{"runs.go", "runs_background.go"}, map[string]bool{"--json": true, "--keep": true, "--timeout": true, "--yes": true}},
		{"skill", []string{"skill.go"}, map[string]bool{"--json": true}},
		{"worktrees", []string{"worktrees.go"}, map[string]bool{"--json": true, "--yes": true}},
	}
	flagLiteral := regexp.MustCompile(`"--[a-z][a-z-]*"`)
	for _, tc := range cases {
		t.Run(tc.help, func(t *testing.T) {
			got := make(map[string]bool)
			for _, source := range tc.sources {
				data, err := os.ReadFile(source)
				if err != nil {
					t.Fatalf("read %s: %v", source, err)
				}
				for _, match := range flagLiteral.FindAllString(string(data), -1) {
					got[strings.Trim(match, `"`)] = true
				}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%s flags in %v = %v, want %v", tc.help, tc.sources, got, tc.want)
			}
			for flag := range tc.want {
				if !strings.Contains(commandHelp[tc.help], flag) {
					t.Errorf("commandHelp[%s] is missing flag %s", tc.help, flag)
				}
			}
		})
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

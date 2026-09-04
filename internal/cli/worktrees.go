package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
)

// worktreesOptions carries one parsed `pi-worker worktrees` invocation.
// Only the list subcommand exists today, so command holds no state of
// its own beyond the one boolean the rendering will branch on; the
// field stays because a future subcommand will set it, and keeping the
// shape mirrors the sibling command families.
type worktreesOptions struct {
	command string
	json    bool
}

const worktreesSchemaVersion = 1

type worktreeListEntry struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Branch string `json:"branch"`
	Dirty  bool   `json:"dirty"`
	Merged bool   `json:"merged"`
}

func worktreesListCommand(parent context.Context, opts worktreesOptions, stdout, stderr io.Writer) int {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: determine workspace: %v\n", err)
		return 9
	}
	worktrees, err := listManagedWorktrees(parent, cwd)
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: list worktrees: %v\n", err)
		return 9
	}
	if worktrees == nil {
		worktrees = []managedWorktree{}
	}
	if opts.json {
		entries := make([]worktreeListEntry, 0, len(worktrees))
		for _, w := range worktrees {
			entries = append(entries, worktreeListEntry{Name: w.name, Path: w.path, Branch: w.branch, Dirty: w.dirty, Merged: w.merged})
		}
		output := struct {
			SchemaVersion int                 `json:"schemaVersion"`
			Worktrees     []worktreeListEntry `json:"worktrees"`
		}{
			SchemaVersion: worktreesSchemaVersion,
			Worktrees:     entries,
		}
		data, err := json.Marshal(output)
		if err != nil {
			fmt.Fprintf(stderr, "pi-worker: encode worktrees: %v\n", err)
			return 9
		}
		fmt.Fprintln(stdout, string(data))
		return 0
	}
	if len(worktrees) == 0 {
		fmt.Fprintln(stdout, "no managed worktrees")
		return 0
	}
	renderWorktreeTable(stdout, worktrees)
	return 0
}

func renderWorktreeTable(w io.Writer, worktrees []managedWorktree) {
	tab := tabwriter.NewWriter(w, 0, 4, 4, ' ', 0)
	fmt.Fprintln(tab, "NAME\tPATH\tBRANCH\tDIRTY\tMERGED")
	for _, wt := range worktrees {
		fmt.Fprintf(tab, "%s\t%s\t%s\t%t\t%t\n", wt.name, wt.path, wt.branch, wt.dirty, wt.merged)
	}
	tab.Flush()
}

// parseWorktreesArgs parses the `pi-worker worktrees` argv. The only
// accepted form is `list`, optionally followed by the single --json
// flag; --json is value-less, so `--json=1` is refused, and nothing
// else is accepted. Every refusal is an ordinary descriptive error:
// the top-level exit handling and usage printing belong to the command
// wiring in a later task.
func parseWorktreesArgs(args []string) (worktreesOptions, error) {
	opts := worktreesOptions{}
	if len(args) == 0 {
		return opts, errors.New("worktrees requires a subcommand")
	}
	switch args[0] {
	case "list":
		opts.command = args[0]
	default:
		return opts, fmt.Errorf("unknown worktrees command %q", args[0])
	}

	seen := map[string]bool{}
	for i := 1; i < len(args); i++ {
		arg := args[i]
		name, _, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--json":
			if hasValue {
				return opts, fmt.Errorf("flag %s does not take a value", name)
			}
			if seen[name] {
				return opts, fmt.Errorf("flag %s specified more than once", name)
			}
			seen[name] = true
			opts.json = true
		default:
			if strings.HasPrefix(arg, "-") {
				return opts, fmt.Errorf("unknown flag %q", arg)
			}
			return opts, fmt.Errorf("unexpected argument %q", arg)
		}
	}

	return opts, nil
}

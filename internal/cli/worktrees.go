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
type worktreesOptions struct {
	command string
	name    string
	json    bool
	yes     bool
}

const worktreesSchemaVersion = 1

type worktreeListEntry struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Branch string `json:"branch"`
	Dirty  bool   `json:"dirty"`
	Merged bool   `json:"merged"`
}

func worktreesCommand(parent context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	opts, err := parseWorktreesArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: %v\n", err)
		printUsage(stderr)
		return 2
	}
	switch opts.command {
	case "list":
		return worktreesListCommand(parent, opts, stdout, stderr)
	case "remove":
		return worktreesRemoveCommand(parent, opts, stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "pi-worker: unknown worktrees command %q\n", opts.command)
		printUsage(stderr)
		return 2
	}
}

func worktreesRemoveCommand(parent context.Context, opts worktreesOptions, _ io.Reader, stdout, stderr io.Writer) int {
	if !opts.yes {
		fmt.Fprint(stderr, "pi-worker: worktrees remove needs --yes when it cannot ask\n")
		return 2
	}
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
	var selected *managedWorktree
	for i := range worktrees {
		if worktrees[i].name == opts.name {
			v := worktrees[i]
			selected = &v
			break
		}
	}
	if selected == nil {
		fmt.Fprintf(stderr, "pi-worker: worktree %q not found\n", opts.name)
		return 2
	}
	if selected.dirty {
		fmt.Fprintf(stderr, "pi-worker: refuse to remove worktree %q: checkout is dirty\n", selected.name)
		return 2
	}
	if !selected.merged {
		fmt.Fprintf(stderr, "pi-worker: refuse to remove worktree %q: branch %q is not merged\n", selected.name, selected.branch)
		return 2
	}
	if err := removeManagedWorktree(parent, cwd, *selected); err != nil {
		fmt.Fprintf(stderr, "pi-worker: %v\n", err)
		return 9
	}
	if opts.json {
		output := struct {
			SchemaVersion int `json:"schemaVersion"`
			Removed       struct {
				Name   string `json:"name"`
				Path   string `json:"path"`
				Branch string `json:"branch"`
			} `json:"removed"`
		}{
			SchemaVersion: worktreesSchemaVersion,
		}
		output.Removed.Name = selected.name
		output.Removed.Path = selected.path
		output.Removed.Branch = selected.branch
		data, err := json.Marshal(output)
		if err != nil {
			fmt.Fprintf(stderr, "pi-worker: encode worktrees: %v\n", err)
			return 9
		}
		fmt.Fprintln(stdout, string(data))
		return 0
	}
	fmt.Fprintf(stdout, "removed worktree %q on branch %q\n", selected.name, selected.branch)
	return 0
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

// parseWorktreesArgs parses the `pi-worker worktrees` argv. Accepted
// forms are `list [--json]` and `remove <name> [--yes] [--json]` where
// the name is validated with validWorktreeName and the two value-less
// flags may appear in either order after the name. Every refusal is an
// ordinary descriptive error: the top-level exit handling and usage
// printing belong to the command wiring.
func parseWorktreesArgs(args []string) (worktreesOptions, error) {
	opts := worktreesOptions{}
	if len(args) == 0 {
		return opts, errors.New("worktrees requires a subcommand")
	}
	switch args[0] {
	case "list":
		opts.command = args[0]
	case "remove":
		opts.command = args[0]
	default:
		return opts, fmt.Errorf("unknown worktrees command %q", args[0])
	}

	if opts.command == "list" {
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
			case "--yes":
				return opts, fmt.Errorf("flag --yes is not valid with worktrees list")
			default:
				if strings.HasPrefix(arg, "-") {
					return opts, fmt.Errorf("unknown flag %q", arg)
				}
				return opts, fmt.Errorf("unexpected argument %q", arg)
			}
		}
		return opts, nil
	}

	if len(args) < 2 {
		return opts, errors.New("worktrees remove requires a name")
	}
	nameArg := args[1]
	if strings.HasPrefix(nameArg, "-") {
		return opts, errors.New("worktrees remove requires a name")
	}
	if !validWorktreeName(nameArg) {
		return opts, fmt.Errorf("invalid worktree name %q: use 1 to 64 characters of lowercase letters, digits and hyphens, starting and ending with a letter or digit", nameArg)
	}
	opts.name = nameArg

	seen := map[string]bool{}
	for i := 2; i < len(args); i++ {
		arg := args[i]
		name, _, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--json", "--yes":
			if hasValue {
				return opts, fmt.Errorf("flag %s does not take a value", name)
			}
			if seen[name] {
				return opts, fmt.Errorf("flag %s specified more than once", name)
			}
			seen[name] = true
			if name == "--json" {
				opts.json = true
			} else {
				opts.yes = true
			}
		default:
			if strings.HasPrefix(arg, "-") {
				return opts, fmt.Errorf("unknown flag %q", arg)
			}
			return opts, fmt.Errorf("unexpected argument %q", arg)
		}
	}

	return opts, nil
}

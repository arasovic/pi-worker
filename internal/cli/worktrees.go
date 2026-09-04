package cli

import (
	"errors"
	"fmt"
	"strings"
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

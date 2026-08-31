package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/arasovic/pi-worker/internal/runlog"
)

// runsSchemaVersion is the runs list document's own version: the
// shape of the {"schemaVersion":1,"runs":[...]} document the --json
// output renders, not the run record's version and not the run
// result's — each document versions itself.
const runsSchemaVersion = 1

type runsOptions struct {
	command string
	json    bool
}

// runsCommand executes read-only run record reporting commands. The
// list is an inventory: it reads records, prints them, and writes
// nothing anywhere — in particular it never touches reported.json,
// never advances the interrupted-run watermark, and never prints an
// interrupted-run warning. Sharing the classification rules with the
// warning is the point; sharing its bookkeeping is a bug.
func runsCommand(parent context.Context, args []string, stdout, stderr io.Writer) int {
	opts, err := parseRunsArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: %v\n", err)
		printUsage(stderr)
		return 2
	}

	switch opts.command {
	case "list":
		return runsListCommand(parent, opts, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "pi-worker: unknown runs command %q\n", opts.command)
		printUsage(stderr)
		return 2
	}
}

func runsListCommand(parent context.Context, opts runsOptions, stdout, stderr io.Writer) int {
	dir, err := runlogDir()
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: determine records directory: %v\n", err)
		return 9
	}
	runs, err := runlogList(dir)
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: list runs: %v\n", err)
		return 9
	}
	if runs == nil {
		// The empty document is an empty array, never null, whatever
		// the reader returned.
		runs = []runlog.Run{}
	}

	if opts.json {
		output := struct {
			SchemaVersion int          `json:"schemaVersion"`
			Runs          []runlog.Run `json:"runs"`
		}{
			SchemaVersion: runsSchemaVersion,
			Runs:          runs,
		}
		data, err := json.Marshal(output)
		if err != nil {
			fmt.Fprintf(stderr, "pi-worker: encode runs: %v\n", err)
			return 9
		}
		fmt.Fprintln(stdout, string(data))
		return 0
	}

	if len(runs) == 0 {
		fmt.Fprintln(stdout, "no runs recorded")
		return 0
	}
	// One line per run, columns aligned; the reader already returned
	// the runs newest first.
	w := tabwriter.NewWriter(stdout, 0, 4, 4, ' ', 0)
	fmt.Fprintln(w, "RUN ID\tSTARTED\tOUTCOME\tTASKS\tWORKSPACE")
	for _, run := range runs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", run.RunID, run.StartedAt, run.Outcome, run.Tasks, run.Workspace)
	}
	w.Flush()
	return 0
}

func parseRunsArgs(args []string) (runsOptions, error) {
	opts := runsOptions{}
	if len(args) == 0 {
		return opts, errors.New("runs requires a subcommand")
	}
	switch args[0] {
	case "list":
		opts.command = args[0]
	default:
		return opts, fmt.Errorf("unknown runs command %q", args[0])
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

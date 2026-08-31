package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/arasovic/pi-worker/internal/runlog"
)

// runsSchemaVersion is the runs list document's own version: the
// shape of the {"schemaVersion":1,"runs":[...]} document the --json
// output renders, not the run record's version and not the run
// result's — each document versions itself.
const runsSchemaVersion = 1

// pruneGraceWindow is how recent an unclassifiable record's file must
// be to count as possibly still being written. A record that was
// modified within the last hour and cannot be read may be a run that
// just created its record and has not yet written its start line; no
// record is legitimately left half-written for an hour, so an older
// unreadable record is still exactly the junk prune exists to clear.
const pruneGraceWindow = time.Hour

type runsOptions struct {
	command string
	json    bool
	// yes answers the deletion prompt up front: prune runs without
	// asking, whatever the terminal state is.
	yes bool
	// keep is the prune cutoff: the newest keep entries stay, whatever
	// their outcome. keepSet reports that the flag appeared, because 0
	// is a legal value and "absent" must stay distinguishable from it.
	keep    int
	keepSet bool
}

// runsCommand executes the run record commands. runs list is the
// read-only inventory; runs prune deletes records — the first code in
// the product that removes anything a run did not itself create — and
// carries its own, careful, delete path in runsPruneCommand.
func runsCommand(parent context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	opts, err := parseRunsArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: %v\n", err)
		printUsage(stderr)
		return 2
	}

	switch opts.command {
	case "list":
		return runsListCommand(parent, opts, stdout, stderr)
	case "prune":
		return runsPruneCommand(parent, opts, stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "pi-worker: unknown runs command %q\n", opts.command)
		printUsage(stderr)
		return 2
	}
}

// runsListCommand executes the read-only runs list reporting command.
// The list is an inventory: it reads records, prints them, and writes
// nothing anywhere — in particular it never touches reported.json,
// never advances the interrupted-run watermark, and never prints an
// interrupted-run warning. Sharing the classification rules with the
// warning is the point; sharing its bookkeeping is a bug.
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
	renderRunTable(stdout, runs)
	return 0
}

// renderRunTable writes the aligned one-line-per-run table both runs
// commands display: the header row and one row per run, newest first
// as the reader returned them. runs prune renders it to stderr so the
// prompt shows what it is about to delete before it asks.
func renderRunTable(w io.Writer, runs []runlog.Run) {
	tab := tabwriter.NewWriter(w, 0, 4, 4, ' ', 0)
	fmt.Fprintln(tab, "RUN ID\tSTARTED\tOUTCOME\tTASKS\tWORKSPACE")
	for _, run := range runs {
		fmt.Fprintf(tab, "%s\t%s\t%s\t%d\t%s\n", run.RunID, run.StartedAt, run.Outcome, run.Tasks, run.Workspace)
	}
	tab.Flush()
}

// runsPruneCommand deletes run records. Selection runs entirely on top
// of runlogList — the same reader runs list uses — so prune and list
// can never disagree about what is a record, what its outcome is, or
// which runs are still alive, and the records directory is never
// walked a second time. This is the first code in pi-worker that
// deletes a user's files; removeRunRecord re-validates every path
// against the directory and the .jsonl rule immediately before the
// removal.
func runsPruneCommand(parent context.Context, opts runsOptions, stdin io.Reader, stdout, stderr io.Writer) int {
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
		runs = []runlog.Run{}
	}

	// The first --keep entries are kept whatever their outcome and are
	// never candidates. Every later entry is a candidate; a candidate
	// whose run is still running is kept and reported separately — a run
	// still writing to its record must never have that record pulled out
	// from under it. A candidate too unreadable to classify is spared
	// the same way while its file is fresh: a record modified within the
	// grace window may be in the middle of being written. Every other
	// candidate is deleted, stale unknown ones included: an unreadable
	// record is exactly the junk this command exists to clear.
	keptNewest := opts.keep
	if keptNewest > len(runs) {
		keptNewest = len(runs)
	}
	var toDelete []runlog.Run
	var keptRunning []runlog.Run
	for i, run := range runs {
		if i < opts.keep {
			continue
		}
		if run.Outcome == "running" {
			keptRunning = append(keptRunning, run)
			continue
		}
		if run.Outcome == "unknown" && recordRecentlyModified(run.Path) {
			keptRunning = append(keptRunning, run)
			continue
		}
		toDelete = append(toDelete, run)
	}
	keptRunningIDs := make([]string, 0, len(keptRunning))
	for _, run := range keptRunning {
		keptRunningIDs = append(keptRunningIDs, run.RunID)
	}

	// --json without --yes refuses up front, before the
	// nothing-selected shortcut: a --json caller is never handed a
	// prompt, and the promise "always with --json without --yes, prune
	// refuses, deletes nothing, and exits 2" must hold whatever the
	// selection is — an empty selection must not turn a refusal into a
	// success.
	if !opts.yes && opts.json {
		fmt.Fprintln(stderr, "pi-worker: runs prune needs --yes when it cannot ask")
		return 2
	}

	// Nothing selected is not an error: a missing or empty records
	// directory lists no runs, and every later record may belong to a
	// still-running run. There is nothing to ask about, so neither --yes
	// nor a terminal is required.
	if len(toDelete) == 0 {
		if opts.json {
			return renderPruneDocument(stdout, stderr, nil, keptRunningIDs, keptNewest)
		}
		fmt.Fprintln(stdout, "nothing to prune")
		return 0
	}

	// The records directory is resolved exactly once, here, before the
	// question is asked, and every deletion goes through that one
	// handle by bare name. os.Remove resolves every parent component of
	// the path it is given, so a full-path remove could be redirected
	// at a file the listing never named by a records directory swapped
	// before the removal; a remove relative to this handle cannot be
	// redirected by any swap that comes after the handle is taken —
	// during the question, or mid-delete — because nothing after this
	// point resolves the directory again. os.OpenRoot follows a symlink
	// in the name it is given, so a deliberately symlinked records
	// directory keeps working. The root is opened only now that there
	// is something to delete, so a missing records directory with
	// nothing to prune behaves exactly as it did before, and it is
	// closed on the way out.
	//
	// One window stays open, and is accepted by design, in the style of
	// the limits in internal/runlog/interrupted.go:
	//
	//   - The directory can still be swapped between runlogList's walk
	//     above and this open. No human wait sits in that window —
	//     nothing between the listing and the open blocks on a person —
	//     and the open itself resolves whatever the name points at
	//     then, so a swap there would send the deletes at the
	//     swapped-in directory's files under the listed names.
	root, err := os.OpenRoot(dir)
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: open records directory: %v\n", err)
		return 9
	}
	defer root.Close()

	// Without --yes the deletion must be approved. A non-terminal stdin
	// cannot answer a prompt, so it refuses verbatim rather than ask
	// into the void; the --json arm of the refusal already returned
	// above.
	if !opts.yes {
		if !stdinIsTerminal() {
			fmt.Fprintln(stderr, "pi-worker: runs prune needs --yes when it cannot ask")
			return 2
		}
		// The prompt shows exactly what it is about to delete before it
		// asks, and both the listing and the question go to stderr: a
		// person who redirected stdout must still see the question they
		// are expected to answer.
		renderRunTable(stderr, toDelete)
		fmt.Fprintf(stderr, "delete %d run records? [y/N] ", len(toDelete))
		answer, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			// An unreadable stdin counts as no answer.
			answer = ""
		}
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "y" && answer != "yes" {
			// Any other answer — n, an empty line, an EOF — deletes
			// nothing: the user got the outcome they asked for.
			fmt.Fprintln(stdout, "nothing deleted")
			return 0
		}
	}

	// The context is read immediately after the prompt returned (or the
	// --yes shortcut past it) and again before every individual delete
	// below: a Ctrl-C that lands before the first delete stops the prune
	// before anything is removed, and one that lands mid-prune stops the
	// deletes where it landed. The prompt itself stays uninterruptible
	// while it blocks on stdin — interrupting that read needs a
	// goroutine and a select over the context and the reader, which is
	// out of scope — so these checks on either side of it are where a
	// cancellation lands.
	if parent.Err() != nil {
		return cancelledPrune(stdout, stderr, opts, nil, keptRunningIDs, keptNewest)
	}

	// Each candidate is deleted one at a time, oldest first, and a
	// failure never stops the others: every selected record is tried,
	// each failure is reported on stderr, and the exit code is 9 at the
	// end. Records already deleted stay deleted, which the deleted lines
	// below say.
	code := 0
	deletedIDs := make([]string, 0, len(toDelete))
	for i := len(toDelete) - 1; i >= 0; i-- {
		// A cancelled context stops the prune before this delete: what
		// was already deleted stays deleted and is reported, nothing
		// further is removed, and the exit is 9.
		if parent.Err() != nil {
			return cancelledPrune(stdout, stderr, opts, deletedIDs, keptRunningIDs, keptNewest)
		}
		run := toDelete[i]
		if err := removeRunRecord(root, dir, run.Path); err != nil {
			fmt.Fprintf(stderr, "pi-worker: delete %s: %v\n", run.RunID, err)
			code = 9
			continue
		}
		deletedIDs = append(deletedIDs, run.RunID)
		if !opts.json {
			fmt.Fprintf(stdout, "deleted %s\n", run.RunID)
		}
	}

	if opts.json {
		if docCode := renderPruneDocument(stdout, stderr, deletedIDs, keptRunningIDs, keptNewest); docCode != 0 {
			return docCode
		}
		return code
	}
	// The summary is one line; the still-running clause appears only
	// when at least one running run was spared, and neither noun is
	// pluralised.
	fmt.Fprintf(stdout, "kept %d newest", keptNewest)
	if len(keptRunning) > 0 {
		fmt.Fprintf(stdout, ", %d still running", len(keptRunning))
	}
	fmt.Fprintln(stdout)
	return code
}

// pruneDocument is the runs prune JSON document's shape. It is this
// command family's own document and versions itself with
// runsSchemaVersion, like runs list's.
type pruneDocument struct {
	SchemaVersion int      `json:"schemaVersion"`
	Deleted       []string `json:"deleted"`
	KeptNewest    int      `json:"keptNewest"`
	KeptRunning   []string `json:"keptRunning"`
}

// cancelledPrune reports a prune stopped by a finished context:
// nothing further is deleted, what was already deleted stays deleted
// and is reported — the human mode printed one deleted line per record
// as it went, and the --json document carries the deleted ids — and
// the exit is 9 with the verbatim message on stderr, the same exit
// code a delete failure uses.
func cancelledPrune(stdout, stderr io.Writer, opts runsOptions, deleted, keptRunning []string, keptNewest int) int {
	if opts.json {
		if docCode := renderPruneDocument(stdout, stderr, deleted, keptRunning, keptNewest); docCode != 0 {
			return docCode
		}
	}
	fmt.Fprintln(stderr, "pi-worker: runs prune cancelled")
	return 9
}

// renderPruneDocument writes the prune document: one line on stdout,
// and the two arrays always arrays — empty, never null, whether
// nothing was deleted or no running run was spared.
func renderPruneDocument(stdout, stderr io.Writer, deleted, keptRunning []string, keptNewest int) int {
	if deleted == nil {
		deleted = []string{}
	}
	if keptRunning == nil {
		keptRunning = []string{}
	}
	output := pruneDocument{
		SchemaVersion: runsSchemaVersion,
		Deleted:       deleted,
		KeptNewest:    keptNewest,
		KeptRunning:   keptRunning,
	}
	data, err := json.Marshal(output)
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: encode prune: %v\n", err)
		return 9
	}
	fmt.Fprintln(stdout, string(data))
	return 0
}

// recordRecentlyModified reports whether the record's file exists and
// was modified within the grace window. Only an existing, fresh file
// can be a record in the middle of being written; a file that cannot
// be stated is treated as before — stale, and deleted by the caller,
// which is exactly the fail-safe direction the junk-clearing behaviour
// pins.
func recordRecentlyModified(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) <= pruneGraceWindow
}

// removeRunRecord deletes one record after re-validating the two facts
// the delete is allowed to rely on, against the exact path being
// removed even though runlogList already produced it: the path is
// inside the records directory — compared lexically, so a symlink is
// never followed, and the removal removes a link itself, never its
// target — and its name ends in .jsonl. The reader's filter is the
// primary guard; this is cheap defence in depth for the product's
// first delete, and it means reported.json, a .reported.json.tmp-*
// stage, a directory, or any other file can never be reached here.
// The removal goes through the caller's single opened root, by bare
// name: os.Remove would resolve every parent component of the full
// path, so a records directory swapped after selection could redirect
// it at a file prune never listed, while a remove relative to the
// handle is pinned to the directory resolved before the first delete.
func removeRunRecord(root *os.Root, dir, path string) error {
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("refusing to delete %q: outside the records directory", path)
	}
	if !strings.HasSuffix(path, ".jsonl") {
		return fmt.Errorf("refusing to delete %q: not a .jsonl record", path)
	}
	return root.Remove(filepath.Base(path))
}

func parseRunsArgs(args []string) (runsOptions, error) {
	opts := runsOptions{}
	if len(args) == 0 {
		return opts, errors.New("runs requires a subcommand")
	}
	switch args[0] {
	case "list", "prune":
		opts.command = args[0]
	default:
		return opts, fmt.Errorf("unknown runs command %q", args[0])
	}

	seen := map[string]bool{}
	for i := 1; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := strings.Cut(arg, "=")
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
			// Value-less exactly like --json: an answered prompt has no
			// value of its own.
			if hasValue {
				return opts, fmt.Errorf("flag %s does not take a value", name)
			}
			if seen[name] {
				return opts, fmt.Errorf("flag %s specified more than once", name)
			}
			seen[name] = true
			opts.yes = true
		case "--keep":
			// Valued exactly like --timeout: both spellings --keep 3 and
			// --keep=3 are accepted, and a repeat is rejected with the
			// same wording.
			if !hasValue {
				if i+1 >= len(args) {
					return opts, fmt.Errorf("flag %s requires a value", name)
				}
				i++
				value = args[i]
			}
			if seen[name] {
				return opts, fmt.Errorf("flag %s specified more than once", name)
			}
			seen[name] = true
			keep, err := strconv.Atoi(value)
			if err != nil || keep < 0 {
				return opts, fmt.Errorf("invalid keep %q: must be a non-negative integer", value)
			}
			opts.keep = keep
			opts.keepSet = true
		default:
			if strings.HasPrefix(arg, "-") {
				return opts, fmt.Errorf("unknown flag %q", arg)
			}
			return opts, fmt.Errorf("unexpected argument %q", arg)
		}
	}

	switch opts.command {
	case "prune":
		if !opts.keepSet {
			return opts, errors.New("runs prune requires --keep <n>")
		}
	case "list":
		// The two prune-only flags stay prune-only: runs list --keep 3
		// is a usage error, not a silently ignored flag.
		if opts.keepSet {
			return opts, fmt.Errorf("flag --keep is not valid with runs list")
		}
		if opts.yes {
			return opts, fmt.Errorf("flag --yes is not valid with runs list")
		}
	}

	return opts, nil
}

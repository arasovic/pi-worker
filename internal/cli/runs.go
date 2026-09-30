package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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
const pruneGraceWindow = runlog.UnknownGrace

type runsOptions struct {
	command string
	json    bool
	// debug streams a waited run's debug file to stderr while it waits.
	// Only runs wait takes it.
	debug bool
	// runID is the identity a status, wait, or cancel asks about. These
	// commands take exactly one, and take it by name: an identity no run is
	// recorded under is a usage error, not a state to report.
	runID string
	// timeout is a wait's own bound: how long it may keep reading a run
	// before it reports the latest state it saw and stops waiting. It is
	// defaulted by the parser, because a wait that named no bound still
	// has one.
	timeout time.Duration
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
	case "status":
		return runsStatusCommand(parent, opts, stdout, stderr)
	case "wait":
		return runsWaitCommand(parent, opts, stdout, stderr)
	case "cancel":
		return runsCancelCommand(parent, opts, stdout, stderr)
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
	bgRoot, err := backgroundRoot()
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: list background runs: %v\n", err)
		return 9
	}
	runs, _, err := listRunInventory(dir, bgRoot)
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: %v\n", err)
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

// runArtifact is one place a run was found: a run directory — read by
// the background reader, either inside the records directory or in the
// legacy background root — or a flat .jsonl record in the records
// directory.
type runArtifact struct {
	runlog.Run
	dir bool
}

// listRunInventory is the one listing runs list shows and runs prune
// selects from: one entry per run id, newest first, plus every artifact
// each id was found under. Three readers feed it — the run directories
// inside the records directory, the legacy background root, and the
// flat records — and an id found more than once is listed once, from
// the first of them in that order: a run directory carries the snapshot
// runs status and wait read, it turns terminal before the record's
// finish line is written, and the newer layout wins over the legacy
// one. The artifacts map keeps every place an id was found, each path
// once, directories first, so a prune of the run removes all of them.
func listRunInventory(dir, bgRoot string) ([]runlog.Run, map[string][]runArtifact, error) {
	flatRuns, err := runlogList(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("list runs: %w", err)
	}
	// The directory reader skips everything that is not a directory
	// named by a run id, so the flat records and reported.json inside
	// the records directory are not read twice.
	dirRuns, err := backgroundListRuns(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("list run directories: %w", err)
	}
	bgRuns, err := backgroundListRuns(bgRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("list background runs: %w", err)
	}
	var runs []runlog.Run
	artifacts := make(map[string][]runArtifact)
	seenPath := make(map[string]bool)
	for _, source := range []struct {
		runs []runlog.Run
		dir  bool
	}{{dirRuns, true}, {bgRuns, true}, {flatRuns, false}} {
		for _, r := range source.runs {
			if _, listed := artifacts[r.RunID]; !listed {
				runs = append(runs, r)
			}
			if seenPath[r.Path] {
				continue
			}
			seenPath[r.Path] = true
			artifacts[r.RunID] = append(artifacts[r.RunID], runArtifact{Run: r, dir: source.dir})
		}
	}
	// Each reader returns its own entries newest first; the merged slice
	// is sorted again so the interleaving is chronological. Ids are
	// unique after the merge, so the order is total.
	sort.Slice(runs, func(i, j int) bool { return runs[i].RunID > runs[j].RunID })
	return runs, artifacts, nil
}

// runsPruneCommand deletes runs. Selection starts on top of
// listRunInventory — the same listing runs list shows — so prune and
// list can never disagree about what is a run, what its outcome is, or
// which runs are still alive, and --keep counts runs, not files: a run
// found as a run directory and as a flat record is one run, and
// deleting it removes every place it was found. An affirmed
// interactive prune lists the delete candidates again immediately
// before deletion and refuses to delete anything if that second
// deletion list no longer matches the first. This is the first code in
// pi-worker that deletes a user's files; the identity of each selected
// artifact is captured when the candidate is chosen, and
// removeRunRecord and removeRunDir re-validate each one, re-check that
// the name still holds what was listed, and re-ask the grace-window
// question of unknown runs, immediately before the removal.
func runsPruneCommand(parent context.Context, opts runsOptions, stdin io.Reader, stdout, stderr io.Writer) int {
	// Without --yes, both documented non-deletion modes refuse before
	// anything else: JSON callers must never be handed a prompt, and a
	// non-terminal stdin cannot answer one. The flags-only JSON arm is
	// short-circuited before terminal detection, so this remains safe
	// before the records directory is resolved or read.
	if !opts.yes && (opts.json || !stdinIsTerminal()) {
		fmt.Fprintln(stderr, "pi-worker: runs prune needs --yes when it cannot ask")
		return 2
	}

	dir, err := runlogDir()
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: determine records directory: %v\n", err)
		return 9
	}
	bgRoot, err := backgroundRoot()
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: list background runs: %v\n", err)
		return 9
	}
	runs, artifacts, err := listRunInventory(dir, bgRoot)
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: %v\n", err)
		return 9
	}

	// The first --keep entries are kept whatever their outcome and
	// are never candidates. Every later entry is a candidate, and
	// two kinds of candidate are kept and reported apart — a run
	// still writing to its record must never have that record
	// pulled out from under it, and a record too unreadable to
	// classify is kept when its file could be one being written,
	// the same freshness question asked a second time at the moment
	// of each delete because the prompt below can wait on a person —
	// see selectPruneRuns, removeRunRecord, and removeRunDir.
	selection := selectPruneRuns(runs, artifacts, opts.keep)

	// Nothing selected is not an error: a missing or empty records
	// directory lists no runs, and every later record may belong to a
	// still-running run. There is nothing to ask about, so neither
	// --yes nor a terminal is required. But a context cancelled while
	// the listing ran must not be reported as a finished prune: the
	// empty selection still takes the cancelled path — the verbatim
	// message on stderr and exit 9, with nothing claimed deleted, and
	// the --json arm still rendering the empty document the way the
	// cancelled path always reports what was deleted.
	if len(selection.toDelete) == 0 {
		if parent.Err() != nil {
			return cancelledPrune(stdout, stderr, opts, nil, selection.keptRunningIDs, selection.keptUnreadableIDs, selection.keptNewest)
		}
		if opts.json {
			return renderPruneDocument(stdout, stderr, nil, selection.keptRunningIDs, selection.keptUnreadableIDs, selection.keptNewest)
		}
		fmt.Fprintln(stdout, "nothing to prune")
		return 0
	}

	// Each parent a selected run was found in — the records directory,
	// the legacy background root, or both — is resolved exactly once,
	// here, before the question is asked, and every deletion goes
	// through that parent's one handle by bare name. os.Remove resolves every parent component of
	// the path it is given, so a full-path remove could be redirected
	// at a file the listing never named by a records directory swapped
	// before the removal; a remove relative to this handle cannot be
	// redirected by any swap that comes after the handle is taken —
	// during the question, or mid-delete — because nothing after this
	// point resolves the directory again. os.OpenRoot follows a symlink
	// in the name it is given, so a deliberately symlinked records
	// directory keeps working. A root is opened only now that there is
	// something to delete in it, so a missing directory with nothing to
	// prune in it behaves exactly as it did before, and every root is
	// closed on the way out.
	//
	// One window stays open, and is accepted by design, in the style of
	// the limits in internal/runlog/interrupted.go:
	//
	//   - A directory can still be swapped between the listing above
	//     and this open. No human wait sits in that window —
	//     nothing between the listing and the open blocks on a person —
	//     and the open itself resolves whatever the name points at
	//     then, so a swap there would send the deletes at the
	//     swapped-in directory's files under the listed names.
	roots := make(map[string]*os.Root)
	defer func() {
		for _, root := range roots {
			root.Close()
		}
	}()
	for _, run := range selection.toDelete {
		for _, art := range selection.artifacts[run.RunID] {
			parent := pruneParent(dir, bgRoot, art)
			if parent == "" || roots[parent] != nil {
				continue
			}
			root, err := os.OpenRoot(parent)
			if err != nil {
				fmt.Fprintf(stderr, "pi-worker: open %s: %v\n", parent, err)
				return 9
			}
			roots[parent] = root
		}
	}

	if !opts.yes {
		// The prompt shows exactly what it is about to delete before
		// it asks, and both the listing and the question go to stderr:
		// a person who redirected stdout must still see the question
		// they are expected to answer.
		renderRunTable(stderr, selection.toDelete)
		fmt.Fprintf(stderr, "delete %d run records? [y/N] ", len(selection.toDelete))
		// The answer is read on its own goroutine, and the command
		// takes whichever arrives first — the answer or a cancelled
		// context — so a Ctrl-C while the question is on screen ends
		// the prune through the same cancelled path as everywhere
		// else: nothing deleted, the verbatim cancelled message on
		// stderr, exit 9. One limit is accepted by design, in the
		// style of the limits in internal/runlog/interrupted.go:
		//
		//   - The reader goroutine stays blocked on stdin for the
		//     rest of the process's life. Nothing can unblock a read
		//     that is already in the kernel, and the process is
		//     exiting anyway; the goroutine holds nothing the command
		//     still needs — the answer channel is buffered so the
		//     send never blocks, and the command has already
		//     returned.
		answerCh := make(chan string, 1)
		go func() {
			answer, err := bufio.NewReader(stdin).ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				// An unreadable stdin counts as no answer.
				answer = ""
			}
			answerCh <- strings.ToLower(strings.TrimSpace(answer))
		}()
		select {
		case <-parent.Done():
			return cancelledPrune(stdout, stderr, opts, nil, selection.keptRunningIDs, selection.keptUnreadableIDs, selection.keptNewest)
		case answer := <-answerCh:
			if answer != "y" && answer != "yes" {
				// Any other answer — n, an empty line, an EOF —
				// deletes nothing: the user got the outcome they
				// asked for. A context already cancelled when the
				// answer is taken still ends in the cancelled path:
				// a command that was told to stop never reports a
				// finished prune.
				if parent.Err() != nil {
					return cancelledPrune(stdout, stderr, opts, nil, selection.keptRunningIDs, selection.keptUnreadableIDs, selection.keptNewest)
				}
				fmt.Fprintln(stdout, "nothing deleted")
				return 0
			}
		}
	}

	if !opts.yes {
		// The prompt has already shown the first selection. If the
		// answer was affirmative, list the candidates again right
		// before deletion and refuse to delete anything when the
		// ordered deletion list no longer matches.
		if parent.Err() != nil {
			return cancelledPrune(stdout, stderr, opts, nil, selection.keptRunningIDs, selection.keptUnreadableIDs, selection.keptNewest)
		}
		freshRuns, freshArtifacts, err := listRunInventory(dir, bgRoot)
		if parent.Err() != nil {
			return cancelledPrune(stdout, stderr, opts, nil, selection.keptRunningIDs, selection.keptUnreadableIDs, selection.keptNewest)
		}
		if err != nil {
			fmt.Fprintf(stderr, "pi-worker: runs prune retry: %v\n", err)
			return 9
		}
		freshSelection := selectPruneRuns(freshRuns, freshArtifacts, opts.keep)
		if !pruneDeletionSelectionEqual(selection, freshSelection) {
			fmt.Fprintln(stderr, "pi-worker: runs prune retry: selection changed")
			return 9
		}
		selection.keptRunning = freshSelection.keptRunning
		selection.keptUnreadable = freshSelection.keptUnreadable
		selection.keptRunningIDs = freshSelection.keptRunningIDs
		selection.keptUnreadableIDs = freshSelection.keptUnreadableIDs
		selection.keptNewest = freshSelection.keptNewest
	}

	// The context is read immediately after the prompt returned (or
	// the --yes shortcut past it) and again before every individual
	// delete below: a Ctrl-C that lands before the first delete stops
	// the prune before anything is removed, and one that lands
	// mid-prune stops the deletes where it landed. The prompt itself
	// is covered by the select above, which takes a cancelled context
	// while the question is on screen.
	if parent.Err() != nil {
		return cancelledPrune(stdout, stderr, opts, nil, selection.keptRunningIDs, selection.keptUnreadableIDs, selection.keptNewest)
	}

	// Each candidate run is deleted one at a time, oldest first, and a
	// failure never stops the others: every selected run is tried, each
	// failure is reported on stderr, and the exit code is 9 at the end.
	// Runs already deleted stay deleted, which the deleted lines below
	// say. Each artifact's name is re-checked against what was listed
	// immediately before its removal — see removePruneRun — so a run
	// refused there is a failure like any other, and one spared there is
	// reported as kept, not as deleted.
	code := 0
	deletedIDs := make([]string, 0, len(selection.toDelete))
	for i := len(selection.toDelete) - 1; i >= 0; i-- {
		// A cancelled context stops the prune before this delete: what
		// was already deleted stays deleted and is reported, nothing
		// further is removed, and the exit is 9.
		if parent.Err() != nil {
			return cancelledPrune(stdout, stderr, opts, deletedIDs, selection.keptRunningIDs, selection.keptUnreadableIDs, selection.keptNewest)
		}
		run := selection.toDelete[i]
		spared, err := removePruneRun(roots, dir, bgRoot, selection.artifacts[run.RunID], selection.listedFiles)
		if err != nil {
			fmt.Fprintf(stderr, "pi-worker: delete %s: %v\n", run.RunID, err)
			code = 9
			continue
		}
		if spared != "" {
			// A delete-time spare joins the summary count and --json
			// array of its kind, just as a selection-time spare did: a
			// run directory whose owner lock is held now is running, and
			// an artifact that could not be classified and changed
			// within the grace window is unreadable.
			if spared == "running" {
				selection.keptRunning = append(selection.keptRunning, run)
				selection.keptRunningIDs = append(selection.keptRunningIDs, run.RunID)
			} else {
				selection.keptUnreadable = append(selection.keptUnreadable, run)
				selection.keptUnreadableIDs = append(selection.keptUnreadableIDs, run.RunID)
			}
			if !opts.json {
				fmt.Fprintf(stdout, "kept %s\n", run.RunID)
			}
			continue
		}
		deletedIDs = append(deletedIDs, run.RunID)
		if !opts.json {
			fmt.Fprintf(stdout, "deleted %s\n", run.RunID)
		}
	}

	// A cancellation that lands after the last delete must not report
	// the prune as finished: nothing is deleted after it, what was
	// already deleted is already reported — the human mode printed one
	// deleted line per record as it went, and the --json document
	// carries the ids — and the cancelled path adds the verbatim
	// message on stderr and exit 9.
	//
	// One limit is accepted by design, in the style of the limits in
	// internal/runlog/interrupted.go:
	//
	//   - A cancellation is reported up to and including the moment
	//     before the final output — the summary line in human mode,
	//     the document in --json mode — because the check sits
	//     directly before both. A cancellation that arrives during
	//     that final output is not reported: the work is finished and
	//     the result is out, and a command that printed its result
	//     must not then claim it was interrupted.
	if parent.Err() != nil {
		return cancelledPrune(stdout, stderr, opts, deletedIDs, selection.keptRunningIDs, selection.keptUnreadableIDs, selection.keptNewest)
	}

	if opts.json {
		if docCode := renderPruneDocument(stdout, stderr, deletedIDs, selection.keptRunningIDs, selection.keptUnreadableIDs, selection.keptNewest); docCode != 0 {
			return docCode
		}
		return code
	}
	// The summary is one line; each clause appears only when at least
	// one record of that kind was spared, and no noun is pluralised.
	// The unreadable clause claims only what is known — the record
	// could not be read, and nothing about when — and never calls an
	// unreadable record running.
	fmt.Fprintf(stdout, "kept %d newest", selection.keptNewest)
	if len(selection.keptRunning) > 0 {
		fmt.Fprintf(stdout, ", %d still running", len(selection.keptRunning))
	}
	if len(selection.keptUnreadable) > 0 {
		fmt.Fprintf(stdout, ", %d unreadable", len(selection.keptUnreadable))
	}
	fmt.Fprintln(stdout)
	return code
}

// pruneSelection is one prune's whole selection in runs-newest-first
// order: the runs to delete, the running and unreadable runs spared
// and reported apart — with keptRunningIDs and keptUnreadableIDs
// holding their ids in the same order — every artifact each run was
// found under, the per-path file each delete candidate's artifact was
// when chosen, which removeRunRecord and removeRunDir compare the name
// against at the delete, and keptNewest, the kept count capped at the
// number of listed runs.
type pruneSelection struct {
	toDelete          []runlog.Run
	keptRunning       []runlog.Run
	keptUnreadable    []runlog.Run
	keptRunningIDs    []string
	keptUnreadableIDs []string
	artifacts         map[string][]runArtifact
	listedFiles       map[string]os.FileInfo
	keptNewest        int
}

// selectPruneRuns chooses what a prune deletes and what it spares,
// running entirely on top of listRunInventory. The first keep runs are
// kept whatever their outcome and are never candidates. Every later
// run is a candidate, judged by every artifact it was found under. Two
// kinds of candidate are kept, and they are reported apart. A
// candidate any of whose artifacts is still running is kept and
// reported as running — a run still writing to its record must never
// have that record pulled out from under it, and a background run's
// flat record can still read running after its snapshot turned
// terminal. A candidate with an artifact too unreadable to classify is
// kept and reported separately when that artifact could be one being
// written: either it changed within the grace window, or its timestamp
// could not be read at all — an artifact whose timestamp this reader
// cannot even examine is exactly the one not to delete. The same
// freshness question is asked a second time, at the moment of each
// delete, because the prompt can wait on a person: an artifact stale
// when listed may be freshly changed by the time the delete happens —
// see removeRunRecord and removeRunDir. Every other candidate is
// deleted, stale unknown ones included: an unreadable record is
// exactly the junk this command exists to clear.
func selectPruneRuns(runs []runlog.Run, artifacts map[string][]runArtifact, keep int) pruneSelection {
	var selection pruneSelection
	selection.artifacts = artifacts
	selection.keptNewest = keep
	if selection.keptNewest > len(runs) {
		selection.keptNewest = len(runs)
	}
	// A candidate artifact whose file cannot be looked up here keeps
	// nothing for its path: what delete-time would compare is absent,
	// and the removal refuses any artifact it cannot examine, so the
	// entry-less artifact gets exactly that refusal at the delete.
	selection.listedFiles = make(map[string]os.FileInfo)
	for i, run := range runs {
		if i < keep {
			continue
		}
		running, unreadable := false, false
		for _, art := range artifacts[run.RunID] {
			running = running || art.Outcome == "running"
			unreadable = unreadable || (art.Outcome == "unknown" && recordRecentlyModified(art.Path))
		}
		if running {
			selection.keptRunning = append(selection.keptRunning, run)
			continue
		}
		if unreadable {
			selection.keptUnreadable = append(selection.keptUnreadable, run)
			continue
		}
		selection.toDelete = append(selection.toDelete, run)
		for _, art := range artifacts[run.RunID] {
			if info, err := os.Lstat(art.Path); err == nil {
				selection.listedFiles[art.Path] = info
			}
		}
	}
	selection.keptRunningIDs = make([]string, 0, len(selection.keptRunning))
	for _, run := range selection.keptRunning {
		selection.keptRunningIDs = append(selection.keptRunningIDs, run.RunID)
	}
	selection.keptUnreadableIDs = make([]string, 0, len(selection.keptUnreadable))
	for _, run := range selection.keptUnreadable {
		selection.keptUnreadableIDs = append(selection.keptUnreadableIDs, run.RunID)
	}
	return selection
}

// pruneDeletionSelectionEqual reports whether two selections would
// delete the same runs, in the same order, each through the same
// artifact paths.
func pruneDeletionSelectionEqual(a, b pruneSelection) bool {
	if len(a.toDelete) != len(b.toDelete) {
		return false
	}
	for i := range a.toDelete {
		if a.toDelete[i].Path != b.toDelete[i].Path || a.toDelete[i].RunID != b.toDelete[i].RunID {
			return false
		}
		artsA, artsB := a.artifacts[a.toDelete[i].RunID], b.artifacts[b.toDelete[i].RunID]
		if len(artsA) != len(artsB) {
			return false
		}
		for j := range artsA {
			if artsA[j].Path != artsB[j].Path {
				return false
			}
		}
	}
	return true
}

// pruneDocument is the runs prune JSON document's shape. It is this
// command family's own document and versions itself with
// runsSchemaVersion, like runs list's. The two kept arrays hold
// different classes of spared record: keptRunning the runs still
// running, keptUnreadable the records that could not be read — their
// file changed within the grace window, or its timestamp could not be
// read at all.
type pruneDocument struct {
	SchemaVersion  int      `json:"schemaVersion"`
	Deleted        []string `json:"deleted"`
	KeptNewest     int      `json:"keptNewest"`
	KeptRunning    []string `json:"keptRunning"`
	KeptUnreadable []string `json:"keptUnreadable"`
}

// cancelledPrune reports a prune stopped by a finished context:
// nothing further is deleted, what was already deleted stays deleted
// and is reported — the human mode printed one deleted line per record
// as it went, and the --json document carries the deleted ids — and
// the exit is 9 with the verbatim message on stderr, the same exit
// code a delete failure uses.
func cancelledPrune(stdout, stderr io.Writer, opts runsOptions, deleted, keptRunning, keptUnreadable []string, keptNewest int) int {
	if opts.json {
		if docCode := renderPruneDocument(stdout, stderr, deleted, keptRunning, keptUnreadable, keptNewest); docCode != 0 {
			return docCode
		}
	}
	fmt.Fprintln(stderr, "pi-worker: runs prune cancelled")
	return 9
}

// renderPruneDocument writes the prune document: one line on stdout,
// and the three arrays always arrays — empty, never null, whether
// nothing was deleted, no running run was spared, or nothing unreadable
// was spared.
func renderPruneDocument(stdout, stderr io.Writer, deleted, keptRunning, keptUnreadable []string, keptNewest int) int {
	if deleted == nil {
		deleted = []string{}
	}
	if keptRunning == nil {
		keptRunning = []string{}
	}
	if keptUnreadable == nil {
		keptUnreadable = []string{}
	}
	output := pruneDocument{
		SchemaVersion:  runsSchemaVersion,
		Deleted:        deleted,
		KeptNewest:     keptNewest,
		KeptRunning:    keptRunning,
		KeptUnreadable: keptUnreadable,
	}
	data, err := json.Marshal(output)
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: encode prune: %v\n", err)
		return 9
	}
	fmt.Fprintln(stdout, string(data))
	return 0
}

// recordRecentlyModified reports whether the record's file was
// modified within the grace window: only a fresh file can be a record
// in the middle of being written. The file's own timestamp is the one
// that matters — Lstat, never Stat, so a symlink's target never
// answers for the link — and a file that cannot be looked up reports
// fresh, not stale: a record this reader cannot even examine is
// exactly the one not to delete. Doubt resolves toward silence, never
// toward acting, the same direction every other reader here takes.
func recordRecentlyModified(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return true
	}
	return time.Since(info.ModTime()) <= pruneGraceWindow
}

// removeRunRecord deletes one record after re-validating the facts
// the delete is allowed to rely on, against the exact path being
// removed even though runlogList already produced it: the path is
// inside the records directory — compared lexically, so a symlink is
// never followed, and the removal removes a link itself, never its
// target — and its name ends in .jsonl. The reader's filter is the
// primary guard; this is cheap defence in depth for the product's
// first delete, and it means reported.json, a .reported.json.tmp-*
// stage, or any other file can never be reached here. The removal
// goes through the caller's single opened root, by bare name:
// os.Remove would resolve every parent component of the full path, so
// a records directory swapped after selection could redirect it at a
// file prune never listed, while a remove relative to the handle is
// pinned to the directory resolved before the first delete.
//
// What the name refers to now is then examined through that same
// handle — one Lstat answers the freshness, identity, and type
// questions, in that order — and only after all three pass is the
// name removed:
//
//   - a name that cannot be looked up — the record vanished, or the
//     directory stopped answering — is refused like any other delete
//     failure, and the caller's exit 9 says the run was not deleted;
//   - a record classified unknown at selection whose file was
//     modified within the grace window is spared, exactly as
//     selection would have spared it, and before anything else: the
//     freshness check during selection ran before the question, and
//     a record stale then may be in the middle of being written now.
//     A record touched during the prompt has by definition changed
//     since it was listed, so identity is asked after freshness —
//     asked first, it would turn today's kept into a refusal. The
//     consequence is accepted: a name that now holds a different
//     file — even a directory — is not refused, because neither the
//     identity nor the type question is asked of a spared record;
//     the record is still reported as kept, with exit 0. Sparing
//     destroys nothing, so nothing is risked by asking no more
//     questions. The caller reports it as kept, and sparing never
//     counts as a failure. A record with any other outcome is
//     deleted whatever its modification time;
//   - the file must be the one that was listed when the candidate was
//     chosen — os.SameFile against the info the caller captured at
//     selection, plus an unchanged size and modification time.
//     Anything else is refused: the name no longer holds what was
//     shown. A candidate whose selection-time lookup failed carries
//     no info at all, and the same refusal applies — nothing about
//     the file now at the name was ever shown;
//   - a regular file or a symlink may be removed; anything else — a
//     directory, a device, a socket, a named pipe — is refused by
//     what it is. Removing a symlink removes the link, never its
//     target.
//
// Three ceilings are accepted by design, and each names its own
// failure direction:
//
//   - The name can still be replaced between the Lstat and the
//     Remove: the window is now the few instructions between two
//     syscalls instead of the whole time a person spends reading a
//     prompt, and nothing in it waits on anybody, but POSIX offers no
//     way to unlink a specific inode. A delete that lands in that
//     window removes whatever holds the name then.
//   - A modification time can be forged, and a wall-clock jump or a
//     coarse filesystem timestamp can make a record that was just
//     written look older than the grace window. The grace window is
//     a courtesy to a writer, not a security boundary.
//   - The identity comparisons — the same file, the same size, the
//     same modification time — compare the name against what a
//     lookup can see, never against content. A record rewritten in
//     place during the prompt with different bytes of the same
//     length, whose modification time is then set back, or one
//     written twice within a single tick of a filesystem whose
//     timestamps are coarse, passes all three and is deleted. Closing
//     the ceiling would mean reading and comparing every record's
//     content at the delete. This gate is a courtesy to a writer and
//     a guard against accident, not a defence against someone
//     deliberately hiding a swap. The failure direction: the delete
//     removes a file whose content is no longer what was listed, and
//     the summary reports it under the listed run's id.
func removeRunRecord(root *os.Root, dir string, run runlog.Run, listed os.FileInfo) (spared bool, err error) {
	path := run.Path
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false, fmt.Errorf("refusing to delete %q: outside the records directory", path)
	}
	if !strings.HasSuffix(path, ".jsonl") {
		return false, fmt.Errorf("refusing to delete %q: not a .jsonl record", path)
	}
	base := filepath.Base(path)

	info, err := root.Lstat(base)
	if err != nil {
		return false, fmt.Errorf("refusing to delete %q: cannot be examined: %v", path, err)
	}
	// Freshness first: a record classified unknown at selection whose
	// file is fresh now is a record that may be being written right
	// now, whatever the file looks like — this check must run before
	// the identity check, because a record touched during the prompt
	// has by definition changed since it was listed, and identity
	// would refuse what freshness is meant to keep.
	if run.Outcome == "unknown" && time.Since(info.ModTime()) <= pruneGraceWindow {
		return true, nil
	}
	// Identity: the file under the name must be the file that was
	// listed when the candidate was chosen. A name that holds a
	// different file — a replacement, a freshly written file of the
	// same name, or nothing the selection ever captured — is refused:
	// the delete would remove something prune never showed.
	if listed == nil || !os.SameFile(listed, info) || listed.Size() != info.Size() || !listed.ModTime().Equal(info.ModTime()) {
		return false, fmt.Errorf("refusing to delete %q: no longer the record that was listed", path)
	}
	// Type: a regular file or a symlink may be removed — a symlink
	// removal takes the link, never its target — and anything else
	// under a record's name is refused by what it is.
	if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
		return false, fmt.Errorf("refusing to delete %q: not a regular file or symlink", path)
	}
	if err := root.Remove(base); err != nil {
		return false, err
	}
	return false, nil
}

// pruneParent names the directory whose opened root an artifact is
// removed through: the records directory for a flat record — whose own
// path removeRunRecord checks — and, for a run directory, the
// directory it sits in, which must be the records directory or the
// legacy background root. Any other parent names nothing, and the
// removal refuses the artifact.
func pruneParent(dir, bgRoot string, art runArtifact) string {
	if !art.dir {
		return filepath.Clean(dir)
	}
	parent := filepath.Dir(art.Path)
	if parent == filepath.Clean(dir) || parent == filepath.Clean(bgRoot) {
		return parent
	}
	return ""
}

// removePruneRun deletes every artifact of one run, run directories
// first and the flat record last, each through its parent's root. The
// first artifact spared or refused stops the run there: a run
// directory whose owner lock is held now keeps the run's flat record
// too, and the run is reported once — spared, failed, or deleted —
// never once per artifact.
func removePruneRun(roots map[string]*os.Root, dir, bgRoot string, arts []runArtifact, listed map[string]os.FileInfo) (spared string, err error) {
	for _, art := range arts {
		root := roots[pruneParent(dir, bgRoot, art)]
		if root == nil {
			return "", fmt.Errorf("refusing to delete %q: outside the records directory and the background directory", art.Path)
		}
		if art.dir {
			spared, err = removeRunDir(root, art.Run, listed[art.Path])
		} else {
			var unreadable bool
			unreadable, err = removeRunRecord(root, dir, art.Run, listed[art.Path])
			if unreadable {
				spared = "unreadable"
			}
		}
		if err != nil || spared != "" {
			return spared, err
		}
	}
	return "", nil
}

// pruneRunDirEntry reports whether name is one a run directory may
// hold: what the supervisor writes into it, and a snapshot replacement
// stage a killed supervisor can leave behind.
func pruneRunDirEntry(name string) bool {
	switch name {
	case "record.jsonl", "snapshot.json", "debug.log", runlog.OwnerLockName:
		return true
	}
	return strings.HasPrefix(name, ".snapshot.json.tmp-")
}

// removeRunDir deletes one run directory through its parent's root,
// after the same re-validation removeRunRecord does for a flat record,
// plus two questions only a directory raises. It never removes a tree
// wholesale: it deletes the entries it knows by name and then the
// empty directory, so anything it does not recognise stays.
//
//   - The name must still be a real directory — Lstat, so a symlink is
//     refused by what it is — and a run classified unknown at selection
//     whose directory changed within the grace window is spared as
//     unreadable, before identity is asked, exactly as removeRunRecord
//     spares a fresh unknown record.
//   - The directory is opened as its own root and checked to be the
//     directory the Lstat saw, so a name swapped for a symlink after
//     the Lstat is refused.
//   - When the directory has an owner lock, the lock is taken without
//     waiting and held through the deletes. A lock held by someone
//     else — a run that owns it now, or another prune deleting it — is
//     spared as running, before identity is asked: sparing destroys
//     nothing, so it may come first, as freshness does.
//   - The directory must be the one that was listed: the same file and
//     modification time as at selection. Size is not compared; a
//     directory's size says nothing about its content.
//   - Every entry must be a name the supervisor writes, or a snapshot
//     replacement stage, and a regular file or a symlink. Anything
//     else — a notes file, a subdirectory — refuses the whole
//     directory before a single entry is removed.
//
// One ceiling is accepted by design: an entry can still appear between
// the scan and the final removal of the directory. Nothing then
// removes it; the directory's own removal fails because it is not
// empty, and that failure is reported like any other.
func removeRunDir(parent *os.Root, run runlog.Run, listed os.FileInfo) (spared string, err error) {
	name := filepath.Base(run.Path)
	if _, err := runlog.ParseRunID(name); err != nil {
		return "", fmt.Errorf("refusing to delete %q: not a run directory", run.Path)
	}
	info, err := parent.Lstat(name)
	if err != nil {
		return "", fmt.Errorf("refusing to delete %q: cannot be examined: %v", run.Path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("refusing to delete %q: not a directory", run.Path)
	}
	if run.Outcome == "unknown" && time.Since(info.ModTime()) <= pruneGraceWindow {
		return "unreadable", nil
	}
	runRoot, err := parent.OpenRoot(name)
	if err != nil {
		return "", err
	}
	defer runRoot.Close()
	if now, err := runRoot.Stat("."); err != nil || !os.SameFile(now, info) {
		return "", fmt.Errorf("refusing to delete %q: no longer the run directory that was listed", run.Path)
	}

	var lock *os.File
	lockInfo, err := runRoot.Lstat(runlog.OwnerLockName)
	switch {
	case err == nil:
		if !lockInfo.Mode().IsRegular() {
			return "", fmt.Errorf("refusing to delete %q: its owner lock is not a regular file", run.Path)
		}
		lock, err = runRoot.OpenFile(runlog.OwnerLockName, os.O_RDONLY, 0)
		if err != nil {
			return "", err
		}
		defer lock.Close()
		held, err := runlog.TryLockOwner(lock)
		if err != nil {
			return "", fmt.Errorf("refusing to delete %q: %v", run.Path, err)
		}
		if !held {
			return "running", nil
		}
	case !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("refusing to delete %q: cannot examine its owner lock: %v", run.Path, err)
	}
	if listed == nil || !os.SameFile(listed, info) || !listed.ModTime().Equal(info.ModTime()) {
		return "", fmt.Errorf("refusing to delete %q: no longer the run directory that was listed", run.Path)
	}

	entries, err := fs.ReadDir(runRoot.FS(), ".")
	if err != nil {
		return "", fmt.Errorf("refusing to delete %q: cannot be read: %v", run.Path, err)
	}
	for _, entry := range entries {
		if !pruneRunDirEntry(entry.Name()) || (!entry.Type().IsRegular() && entry.Type()&fs.ModeSymlink == 0) {
			return "", fmt.Errorf("refusing to delete %q: unexpected entry %q", run.Path, entry.Name())
		}
	}
	for _, entry := range entries {
		if entry.Name() == runlog.OwnerLockName {
			continue
		}
		if err := runRoot.Remove(entry.Name()); err != nil {
			return "", err
		}
	}
	if lock != nil {
		if err := runRoot.Remove(runlog.OwnerLockName); err != nil {
			return "", err
		}
		lock.Close()
	}
	return "", parent.Remove(name)
}

func parseRunsArgs(args []string) (runsOptions, error) {
	opts := runsOptions{}
	if len(args) == 0 {
		return opts, errors.New("runs requires a subcommand")
	}
	switch args[0] {
	case "list", "prune", "status", "wait", "cancel":
		opts.command = args[0]
	default:
		return opts, fmt.Errorf("unknown runs command %q", args[0])
	}
	// A wait always has a bound, whether or not the caller named one, so
	// the default is carried from here rather than applied at the read:
	// the number the command reports when it runs out is the number it
	// actually waited to.
	opts.timeout = defaultRunsWaitTimeout

	// Identity arguments, in the order they appeared. Only the two read
	// commands can take one, and only one: a second is a mistake the
	// caller has to fix, never something to pick a side of.
	var identities []string

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
		case "--debug":
			if hasValue {
				return opts, fmt.Errorf("flag %s does not take a value", name)
			}
			if seen[name] {
				return opts, fmt.Errorf("flag %s specified more than once", name)
			}
			seen[name] = true
			opts.debug = true
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
		case "--timeout":
			// Valued exactly like --keep: both spellings --timeout 30s and
			// --timeout=30s are accepted, and a repeat is rejected with the
			// same wording. It is a wait's bound, so the same duration a
			// run's --timeout takes is what it holds a wait to.
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
			duration, err := time.ParseDuration(value)
			if err != nil {
				return opts, fmt.Errorf("invalid timeout %q: %v", value, err)
			}
			if duration <= 0 {
				return opts, fmt.Errorf("invalid timeout %q: must be positive", value)
			}
			opts.timeout = duration
		default:
			if strings.HasPrefix(arg, "-") {
				return opts, fmt.Errorf("unknown flag %q", arg)
			}
			if opts.command == "status" || opts.command == "wait" || opts.command == "cancel" {
				identities = append(identities, arg)
				continue
			}
			return opts, fmt.Errorf("unexpected argument %q", arg)
		}
	}

	if opts.debug && opts.command != "wait" {
		return opts, fmt.Errorf("flag --debug is not valid with runs %s", opts.command)
	}

	switch opts.command {
	case "prune":
		if !opts.keepSet {
			return opts, errors.New("runs prune requires --keep <n>")
		}
		if seen["--timeout"] {
			return opts, fmt.Errorf("flag --timeout is not valid with runs prune")
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
		if seen["--timeout"] {
			return opts, fmt.Errorf("flag --timeout is not valid with runs list")
		}
	case "status", "wait":
		// The prune-only flags and the wait's own bound stay what they
		// are: a status waits for nothing, so it has no bound to set, and
		// neither command deletes anything, so neither takes --keep or
		// --yes.
		if opts.keepSet {
			return opts, fmt.Errorf("flag --keep is not valid with runs %s", opts.command)
		}
		if opts.yes {
			return opts, fmt.Errorf("flag --yes is not valid with runs %s", opts.command)
		}
		if opts.command == "status" && seen["--timeout"] {
			return opts, fmt.Errorf("flag --timeout is not valid with runs status")
		}
		switch len(identities) {
		case 0:
			return opts, fmt.Errorf("runs %s requires a run id", opts.command)
		case 1:
			opts.runID = identities[0]
		default:
			return opts, fmt.Errorf("runs %s takes one run id, got %d", opts.command, len(identities))
		}
	case "cancel":
		// Cancel is a one-shot signal request. It has no wait bound and
		// never takes either of the prune-only flags.
		if opts.keepSet {
			return opts, fmt.Errorf("flag --keep is not valid with runs cancel")
		}
		if opts.yes {
			return opts, fmt.Errorf("flag --yes is not valid with runs cancel")
		}
		if seen["--timeout"] {
			return opts, fmt.Errorf("flag --timeout is not valid with runs cancel")
		}
		switch len(identities) {
		case 0:
			return opts, errors.New("runs cancel requires a run id")
		case 1:
			opts.runID = identities[0]
		default:
			return opts, fmt.Errorf("runs cancel takes one run id, got %d", len(identities))
		}
	}

	return opts, nil
}

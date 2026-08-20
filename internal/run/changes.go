package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Changes is the manifest of workspace paths a run changed, measured by
// pi-worker itself against the before-state HEAD rather than reported by
// the worker; nil when the workspace is not inside a git work tree.
// Paths that were already dirty before the run and whose stamp (size
// plus modification time) did not move are subtracted, so the manifest
// answers what this run changed even on a dirty before-state.
// Unlike GitChange it is not gated by the git tripwire: leaving modified
// files behind is the point of a delegation, and those files are exactly
// what the manifest exists to name.
type Changes struct {
	// Omitted is the reason the manifest could not be measured; empty
	// when it was measured. Files, TotalFiles, and Truncated are
	// meaningful only when Omitted is empty.
	Omitted    string       `json:"omitted,omitempty"`
	Files      []FileChange `json:"files,omitempty"`
	TotalFiles int          `json:"totalFiles"`
	Truncated  bool         `json:"truncated,omitempty"`
	// allPaths is the complete set of changed workspace paths before
	// the cap: the merged manifest set plus the untracked files that
	// were listed but never measured, which still count toward
	// TotalFiles. It is present whenever the manifest was measured and
	// empty when it was omitted, and it backs the post-run write check,
	// which must not decide policy from the capped, churn-ordered
	// manifest list. Unexported, so encoding/json never sees it and
	// schemaVersion stays 1.
	allPaths []string
}

// FileChange is one changed workspace path with its measured line
// counts. Status is exactly one of "added", "modified", "deleted".
type FileChange struct {
	Path    string `json:"path"`
	Status  string `json:"status"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Binary  bool   `json:"binary,omitempty"`
	// DirtyBefore is true when the path was already dirty before the
	// run started: the line counts are measured against the last commit
	// rather than against the pre-run content, so they include work
	// that was already there and the run's share cannot be separated
	// out. It is additive and optional, so schemaVersion stays 1.
	DirtyBefore bool `json:"dirtyBefore,omitempty"`
}

// The file status values in the manifest.
const (
	statusAdded    = "added"
	statusModified = "modified"
	statusDeleted  = "deleted"
)

// Omission reasons. They are short, lowercase, fixed strings, and there
// are exactly these three: an unborn HEAD, a context already done when
// the before-state inspection ran, and a failed measurement (a git
// command failure or a budget that expires).
const (
	reasonUnbornHead      = "unborn head"
	reasonContextDone     = "context already done"
	reasonMeasurementFail = "measurement failed"
)

// maxChangeFiles caps the manifest list. TotalFiles always carries the
// true count and Truncated is true when the cap dropped entries, so a
// run touching a thousand files never produces an unbounded document.
const maxChangeFiles = 100

// changesTimeout bounds the whole manifest measurement, including the
// untracked pass that can spawn one no-index diff command per file up
// to the cap. It is a fresh budget of its own, never the after state's
// five seconds and never the parent context, because a run that timed
// out mid-edit is exactly the run whose changes a caller most needs.
const changesTimeout = 30 * time.Second

// measureChanges measures the workspace change manifest against the
// before-state HEAD, subtracting the before-dirty paths whose stamps
// did not move during the run, or returns a Changes carrying the reason
// it could not. dirtyStamps is the pre-run snapshot of the paths that
// were already dirty; a measurement failure is reported, never silent:
// leaving the field nil on a failure is the bug this feature exists to
// prevent, so the caller keeps the same distinction as git — a
// workspace outside a git work tree is the one nil case, and it is
// decided by the caller before this function is reached.
func measureChanges(ctx context.Context, dir string, before *GitState, dirtyStamps map[string]fileStamp) *Changes {
	if before.Head == "" {
		return &Changes{Omitted: reasonUnbornHead}
	}
	changes, err := measureChangeFiles(ctx, dir, before.Head, dirtyStamps)
	if err != nil {
		return &Changes{Omitted: reasonMeasurementFail}
	}
	return changes
}

// measureChangeFiles measures the tracked and untracked workspace
// changes against head, minus the before-dirty paths whose stamps did
// not move. The tracked pass is one diff command that
// covers every tracked change including work the run committed, because
// the base is the before-state HEAD rather than the current one. The
// untracked pass lists untracked files with one command and takes each
// one's counts with git's own semantics via git diff --no-index, one
// command per file up to the cap. Every command is read-only: no add
// (including intent-to-add), stash, commit, checkout, reset, clean, or
// branch, tag, or worktree operation. Any parse failure or command
// failure fails the whole measurement: an approximate manifest
// presented as truth is worse than none.
func measureChangeFiles(ctx context.Context, dir, head string, dirtyStamps map[string]fileStamp) (*Changes, error) {
	trackedOut, err := gitOutput(ctx, dir, "diff", "--numstat", "--no-renames", "-z", head, "--")
	if err != nil {
		return nil, fmt.Errorf("git diff: %w", err)
	}
	tracked, err := parseNumstatTracked(trackedOut)
	if err != nil {
		return nil, err
	}
	// The before-tree names the file at the base commit. Counts alone
	// cannot decide a status: a pure insertion into an existing file and
	// a new file both print "+N/-0", and a removed file and a fully
	// emptied file both print "+0/-N", so existence at the base is read
	// from the tree and current presence from the workspace itself.
	beforeTree, err := gitOutput(ctx, dir, "ls-tree", "-r", "--name-only", "-z", head)
	if err != nil {
		return nil, fmt.Errorf("git ls-tree: %w", err)
	}
	existed := make(map[string]bool)
	for _, path := range nulSplit(beforeTree) {
		existed[path] = true
	}
	untrackedOut, err := gitOutput(ctx, dir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	untracked := nulSplit(untrackedOut)

	// The candidate set is the union of three sets: the tracked diff
	// against the before-state HEAD, the untracked listing, and the
	// paths that were dirty before the run. From that union, every
	// before-dirty path whose stamp is unchanged — same size and same
	// modification time, or absent then and absent now — is subtracted:
	// it was equally dirty before the run and the run contributed
	// nothing to it, so it names no change this run made. The dirty
	// union term is not optional: without it a worker that reverts an
	// already-dirty file to its committed content drops out of the HEAD
	// diff completely and the manifest would go silent even though the
	// caller's uncommitted work was destroyed.
	unchanged := make(map[string]bool, len(dirtyStamps))
	for path, stamp := range dirtyStamps {
		matches, err := stampMatches(dir, path, stamp)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}
		if matches {
			unchanged[path] = true
		}
	}

	// Entries are keyed by path so a tracked deletion that the run
	// replaced with an untracked file at the same path merges into one
	// entry: the old file's deletions and the new file's additions.
	// TotalFiles counts distinct paths.
	byPath := make(map[string]int, len(tracked)+len(untracked))
	files := make([]FileChange, 0, len(tracked)+len(untracked))
	appendFile := func(f FileChange) {
		if i, seen := byPath[f.Path]; seen {
			files[i].Added += f.Added
			files[i].Deleted += f.Deleted
			files[i].Binary = files[i].Binary || f.Binary
			if files[i].Status == statusDeleted {
				files[i].Status = statusModified
			}
			return
		}
		byPath[f.Path] = len(files)
		files = append(files, f)
	}
	for _, f := range tracked {
		// A before-dirty path whose stamp did not move is subtracted even
		// from the tracked diff: it was equally dirty before the run.
		if unchanged[f.Path] {
			continue
		}
		switch {
		case !existed[f.Path]:
			f.Status = statusAdded
		default:
			present, err := filePresent(dir, f.Path)
			if err != nil {
				return nil, fmt.Errorf("stat %s: %w", f.Path, err)
			}
			if present {
				f.Status = statusModified
			} else {
				f.Status = statusDeleted
			}
		}
		appendFile(f)
	}
	// The untracked pass can spawn one command per file up to the cap; the
	// files beyond it are listed but their churn stays unmeasured. They
	// still count toward TotalFiles, which is the true count of changed
	// paths before the cap and before this trim. Subtracted before-dirty
	// paths drop out of the listing before the cap: an untouched pre-run
	// file neither takes a measured slot nor counts toward TotalFiles.
	keptUntracked := make([]string, 0, len(untracked))
	unmeasured := []string(nil)
	for _, path := range untracked {
		if unchanged[path] {
			continue
		}
		if len(keptUntracked) == maxChangeFiles {
			unmeasured = append(unmeasured, path)
			continue
		}
		keptUntracked = append(keptUntracked, path)
	}
	for _, path := range keptUntracked {
		added, deleted, binary, err := measureUntrackedFile(ctx, dir, path)
		if err != nil {
			return nil, err
		}
		appendFile(FileChange{Path: path, Status: statusAdded, Added: added, Deleted: deleted, Binary: binary})
	}

	// A before-dirty path whose stamp moved but that appears in neither
	// the tracked diff nor the untracked listing now matches HEAD, or is
	// gone: the run reverted it to its committed content — the case the
	// dirty union exists for — or removed it. Give it an entry with zero
	// counts and dirtyBefore true. Current presence decides first: a
	// path that no longer exists cannot be an addition, so a gone path
	// is deleted even when it was never in the base tree — an untracked
	// file the run deleted was never in the base tree, and calling it
	// added would name a nonexistent path an addition. A present path
	// is added when it was not in the base tree and modified when it
	// was. Every changed path, including these, must reach allPaths,
	// because that is what the write check compares against.
	inTracked := make(map[string]bool, len(tracked))
	for _, f := range tracked {
		inTracked[f.Path] = true
	}
	inUntracked := make(map[string]bool, len(untracked))
	for _, path := range untracked {
		inUntracked[path] = true
	}
	for path := range dirtyStamps {
		if unchanged[path] || inTracked[path] || inUntracked[path] {
			continue
		}
		if _, seen := byPath[path]; seen {
			continue
		}
		f := FileChange{Path: path, DirtyBefore: true}
		present, err := filePresent(dir, path)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}
		switch {
		case !present:
			f.Status = statusDeleted
		case !existed[path]:
			f.Status = statusAdded
		default:
			f.Status = statusModified
		}
		appendFile(f)
	}
	// Every entry whose path was dirty before the run is marked, so a
	// caller knows its counts include work that was already there.
	for path := range dirtyStamps {
		if i, seen := byPath[path]; seen {
			files[i].DirtyBefore = true
		}
	}

	// Tracked entries and the measured untracked entries are ordered by
	// total churn (added + deleted) descending, then by path for a
	// stable order. Untracked files beyond the cap are dropped in
	// listing order without being measured, because measuring churn
	// costs one command per file, so an unmeasured file never appears
	// among the kept entries however large it is. TotalFiles is the
	// true count before the cap.
	total := len(files)
	for _, path := range unmeasured {
		if _, seen := byPath[path]; !seen {
			total++
		}
	}
	// allPaths is built from the same merged set TotalFiles counts, so
	// every untracked file beyond the cap is present even though it was
	// never measured. The write check compares against this complete
	// set, never against the capped list: a run that changed 150 paths
	// and wrote outside its declaration only in path number 130 must
	// still be caught. Ordering does not matter here; the check treats
	// it as a set.
	allPaths := make([]string, 0, total)
	for path := range byPath {
		allPaths = append(allPaths, path)
	}
	for _, path := range unmeasured {
		if _, seen := byPath[path]; !seen {
			allPaths = append(allPaths, path)
		}
	}
	sort.Slice(files, func(i, j int) bool {
		ci, cj := files[i].Added+files[i].Deleted, files[j].Added+files[j].Deleted
		if ci != cj {
			return ci > cj
		}
		return files[i].Path < files[j].Path
	})
	changes := &Changes{Files: files, TotalFiles: total, allPaths: allPaths}
	if total > maxChangeFiles {
		changes.Files = changes.Files[:maxChangeFiles]
		changes.Truncated = true
	}
	return changes, nil
}

// fileStamp is the identity clue captured for one path that was
// already dirty when the run started: size and modification time from
// Lstat, or an explicit absence for a staged or unstaged deletion,
// which is a legitimate dirty state. Size plus modification time is
// deliberate: a worker that writes a file always moves its modification
// time, and the failure direction is the safe one — a missed change
// means a path is left out of the manifest, never that an honest run is
// accused. File contents are never read and git hash-object never runs.
type fileStamp struct {
	size    int64
	modTime time.Time
	absent  bool
}

// snapshotDirtyStamps enumerates the paths already dirty in the
// workspace before the run starts and stamps each one, keyed by path,
// so the manifest can subtract the ones the run never moved.
// Enumerating with exactly the two commands the after pass already uses
// — git diff --name-only -z HEAD -- and git ls-files --others
// --exclude-standard -z — keeps both passes over the same universe.
// Every command here is read-only with respect to the repository.
func snapshotDirtyStamps(ctx context.Context, dir string) (map[string]fileStamp, error) {
	trackedOut, err := gitOutput(ctx, dir, "diff", "--name-only", "-z", "HEAD", "--")
	if err != nil {
		return nil, fmt.Errorf("git diff --name-only: %w", err)
	}
	untrackedOut, err := gitOutput(ctx, dir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	stamps := make(map[string]fileStamp)
	for _, path := range append(nulSplit(trackedOut), nulSplit(untrackedOut)...) {
		info, err := os.Lstat(filepath.Join(dir, path))
		if err != nil {
			if os.IsNotExist(err) {
				stamps[path] = fileStamp{absent: true}
				continue
			}
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}
		stamps[path] = fileStamp{size: info.Size(), modTime: info.ModTime()}
	}
	return stamps, nil
}

// stampMatches reports whether the workspace path currently carries the
// captured pre-run stamp: both present with the same size and the same
// modification time, or absent then and absent now. A path that was not
// absent before but is a directory now has changed — a file replaced by
// a directory is not the same path it was.
func stampMatches(dir, path string, stamp fileStamp) (bool, error) {
	info, err := os.Lstat(filepath.Join(dir, path))
	if err != nil {
		if os.IsNotExist(err) {
			return stamp.absent, nil
		}
		return false, err
	}
	if stamp.absent || info.IsDir() {
		return false, nil
	}
	return info.Size() == stamp.size && info.ModTime().Equal(stamp.modTime), nil
}

// filePresent reports whether the workspace-relative path is currently a
// non-directory entry in the workspace, the ground truth for whether a
// file that existed at the base was deleted or merely emptied.
func filePresent(dir, path string) (bool, error) {
	info, err := os.Lstat(filepath.Join(dir, path))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return !info.IsDir(), nil
}

// measureUntrackedFile takes one untracked file's counts with git's own
// semantics: git diff --numstat --no-index against /dev/null, which is
// a literal new-file comparison. The command exits non-zero when the
// files differ — the normal case here, including an empty new file,
// which still prints a record with zero counts — so a non-zero exit is
// not a failure for this command specifically; an unparseable or
// missing record is.
func measureUntrackedFile(ctx context.Context, dir, path string) (added, deleted int, binary bool, err error) {
	out, err := gitOutputAnyExit(ctx, dir, "diff", "--numstat", "--no-index", "-z", "--", "/dev/null", path)
	if err != nil {
		return 0, 0, false, fmt.Errorf("git diff --no-index %s: %w", path, err)
	}
	parsed, added, deleted, binary, err := parseNumstatNoIndex(out)
	if err != nil {
		return 0, 0, false, fmt.Errorf("git diff --no-index %s: %w", path, err)
	}
	if parsed != path {
		return 0, 0, false, fmt.Errorf("git diff --no-index %s: record names %q", path, parsed)
	}
	return added, deleted, binary, nil
}

// parseNumstatTracked parses git diff --numstat --no-renames -z output
// into one FileChange per NUL-terminated record. The no-renames flag
// keeps every record in the single-path form "<added>\t<deleted>\t<path>"
// with the path written raw up to the NUL — a renamed pair would print a
// combined, abbreviated path and the manifest must carry literal paths.
func parseNumstatTracked(out string) ([]FileChange, error) {
	tracked := []FileChange{}
	for _, record := range strings.Split(out, "\x00") {
		if record == "" {
			continue
		}
		added, deleted, path, binary, err := splitNumstatRecord(record)
		if err != nil {
			return nil, fmt.Errorf("diff record %q: %w", record, err)
		}
		if path == "" {
			return nil, fmt.Errorf("diff record %q: missing path", record)
		}
		tracked = append(tracked, FileChange{Path: path, Added: added, Deleted: deleted, Binary: binary})
	}
	return tracked, nil
}

// parseNumstatNoIndex parses one git diff --numstat --no-index -z
// record. The no-index form is the two-path form: the counts chunk is
// "<added>\t<deleted>\t" (the path follows the NUL, so the rest after
// the second tab is empty) and the NUL-separated path chunks after it
// name /dev/null and the compared file in order; the last non-empty
// chunk is the file's literal path.
func parseNumstatNoIndex(out string) (path string, added, deleted int, binary bool, err error) {
	chunks := strings.Split(out, "\x00")
	if len(chunks) < 3 || chunks[0] == "" {
		return "", 0, 0, false, fmt.Errorf("unparseable no-index record")
	}
	added, deleted, rest, binary, err := splitNumstatRecord(chunks[0])
	if err != nil {
		return "", 0, 0, false, err
	}
	if rest != "" {
		return "", 0, 0, false, fmt.Errorf("unexpected count tail %q", rest)
	}
	path = chunks[len(chunks)-2]
	if path == "" {
		return "", 0, 0, false, fmt.Errorf("missing compared path")
	}
	return path, added, deleted, binary, nil
}

// splitNumstatRecord splits one NUL-terminated numstat record chunk into
// its counts and the remainder after the second tab: the path in the
// tracked single-path form and empty in the no-index two-path form. A
// binary file prints "-" for both counts, which is Binary with both
// counts zero, not a parse error.
func splitNumstatRecord(record string) (added, deleted int, rest string, binary bool, err error) {
	addedStr, rem, ok := strings.Cut(record, "\t")
	if !ok {
		return 0, 0, "", false, fmt.Errorf("missing added count")
	}
	deletedStr, rest, ok := strings.Cut(rem, "\t")
	if !ok {
		return 0, 0, "", false, fmt.Errorf("missing deleted count")
	}
	if addedStr == "-" && deletedStr == "-" {
		return 0, 0, rest, true, nil
	}
	if added, err = strconv.Atoi(addedStr); err != nil {
		return 0, 0, "", false, fmt.Errorf("added count %q", addedStr)
	}
	if deleted, err = strconv.Atoi(deletedStr); err != nil {
		return 0, 0, "", false, fmt.Errorf("deleted count %q", deletedStr)
	}
	return added, deleted, rest, false, nil
}

// nulSplit returns the non-empty NUL-separated fields of output, the
// terminator git emits in -z mode so that paths with spaces, tabs,
// newlines, and non-ASCII bytes are carried literally.
func nulSplit(out string) []string {
	var parts []string
	for _, part := range strings.Split(out, "\x00") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}

// gitOutputAnyExit runs one read-only git command in dir and returns
// its captured stdout whether or not the command exited non-zero; a
// start failure or a context that expires or is cancelled is an error.
// It is the sibling of gitOutput for the commands that exit non-zero as
// their normal outcome, git diff --no-index chief among them.
func gitOutputAnyExit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return stdout.String(), nil
		}
		return stdout.String(), err
	}
	return stdout.String(), nil
}

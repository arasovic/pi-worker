package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
// the worker. The controller decides presence: with a git inspector
// configured the field always carries either a stated omission reason or
// the measurement, and is nil only when no inspector was configured at
// all. Paths that were already dirty before the run and whose stamp
// (size, modification time, and executable bit) did not move are
// subtracted, so the manifest answers what this run changed even on a
// dirty before-state.
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
	// root is the repository root used to measure allPaths. It lets the
	// write check translate workspace-relative declarations into the same
	// coordinate system once at its shared comparison boundary.
	root string
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
	// NoFinalNewline is true when the file's last byte is not a
	// newline. It is a measurement, never a verdict: it makes no claim
	// that the file is wrong and no claim about who produced it, so a
	// modified file may have been like that before the run while an
	// added one was produced that way by the run itself. It is
	// additive and optional, so schemaVersion stays 1.
	NoFinalNewline bool `json:"noFinalNewline,omitempty"`
}

// The file status values in the manifest.
const (
	statusAdded    = "added"
	statusModified = "modified"
	statusDeleted  = "deleted"
)

// Omission reasons. They are short, lowercase, fixed strings, and there
// are exactly these four: an unborn HEAD, a context already done when
// the before-state inspection ran, a failed measurement (a git command
// failure or a budget that expires), and a work tree that could not be
// confirmed — the directory is not a git work tree, git is missing
// entirely, or the guard failed for a transient reason, three causes
// the code cannot tell apart. That last reason names only what is
// known and never claims which of the three it is.
const (
	reasonUnbornHead          = "unborn head"
	reasonContextDone         = "context already done"
	reasonMeasurementFail     = "measurement failed"
	reasonWorkTreeUnconfirmed = "work tree not confirmed"
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
// prevent. The caller has already confirmed the workspace is inside a
// git work tree before this function is reached, so it never states the
// work-tree-unconfirmed reason itself. The measurement is anchored at
// the repository root, resolved once from dir, so every measured path
// is root-relative and the answer is the same whichever directory
// inside the repository the run was started from; the worker's own
// working directory is untouched.
func measureChanges(ctx context.Context, dir string, before *GitState, dirtyStamps map[string]fileStamp) *Changes {
	if before.Head == "" {
		return &Changes{Omitted: reasonUnbornHead}
	}
	root, err := repoRoot(ctx, dir)
	if err != nil {
		return &Changes{Omitted: reasonMeasurementFail}
	}
	changes, err := measureChangeFiles(ctx, root, before.Head, dirtyStamps)
	if err != nil {
		return &Changes{Omitted: reasonMeasurementFail}
	}
	return changes
}

// repoRoot resolves the repository root from dir with git rev-parse
// --show-toplevel, the same command the CLI worktree uses, so the two
// agree on where the repository is. The worker keeps working in the
// directory it was given; only the measurement moves to the root. A
// failure is a git command failure: it fails the measurement like any
// other, never a silent fallback to the caller's directory, because a
// measurement against the wrong base would quietly report the wrong
// paths.
func repoRoot(ctx context.Context, dir string) (string, error) {
	out, err := gitOutput(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("git rev-parse --show-toplevel: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// measureChangeFiles measures the tracked and untracked workspace
// changes against head, minus the before-dirty paths whose stamps did
// not move. root is the repository root resolved once by the caller:
// every command runs with root as its working directory and every
// filesystem read joins against it, so every measured path is
// root-relative, the base every consumer of the manifest already uses.
// The tracked pass is one diff command that
// covers every tracked change including work the run committed, because
// the base is the before-state HEAD rather than the current one, and
// ignores submodules: a submodule's contents are another repository's
// business, so a dirty submodule must not become a changed path. The
// untracked pass lists untracked files with one command and takes each
// one's counts with git's own semantics via git diff --no-index, one
// command per file up to the cap. Every command is read-only: no add
// (including intent-to-add), stash, commit, checkout, reset, clean, or
// branch, tag, or worktree operation. Any parse failure or command
// failure fails the whole measurement: an approximate manifest
// presented as truth is worse than none.
func measureChangeFiles(ctx context.Context, root, head string, dirtyStamps map[string]fileStamp) (*Changes, error) {
	trackedOut, err := gitOutput(ctx, root, "diff", "--numstat", "--no-renames", "--ignore-submodules=all", "-z", head, "--")
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
	beforeTree, err := gitOutput(ctx, root, "ls-tree", "-r", "--name-only", "-z", head)
	if err != nil {
		return nil, fmt.Errorf("git ls-tree: %w", err)
	}
	existed := make(map[string]bool)
	for _, path := range nulSplit(beforeTree) {
		existed[path] = true
	}
	untrackedOut, err := gitOutput(ctx, root, "ls-files", "--others", "--exclude-standard", "-z")
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
		matches, err := stampMatches(root, path, stamp)
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
			present, err := filePresent(root, f.Path)
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
		added, deleted, binary, err := measureUntrackedFile(ctx, root, path)
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
		present, err := filePresent(root, path)
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
	changes := &Changes{Files: files, TotalFiles: total, allPaths: allPaths, root: root}
	if total > maxChangeFiles {
		changes.Files = changes.Files[:maxChangeFiles]
		changes.Truncated = true
	}
	// The field is measured only over the entries actually reported: a
	// path the cap dropped — or an untracked path that was listed but
	// never measured — carries no field at all, exactly as Binary
	// behaves. This is the first place the manifest reads file content
	// at all: fileStamp is deliberately size, modification time and
	// executable bit only, and one byte per listed file is a
	// deliberate, bounded exception.
	for i := range changes.Files {
		measureNoFinalNewline(root, &changes.Files[i])
	}
	return changes, nil
}

// measureNoFinalNewline reads the file's last byte and sets the
// NoFinalNewline field when it is not a newline. The read is one byte,
// never the whole file: os.ReadFile would pull a multi-gigabyte
// artifact into memory to look at its last byte, which is exactly why
// the obvious shorter code is the wrong code here. The field is best
// effort and that is the whole error story: an Lstat failure (a
// deleted file lands here for free), a path that is not a regular file
// (which excludes symlinks — following a symlink measures the target
// rather than the listed entry), a zero-size file, an entry git
// already reported binary, and an open, seek, or read failure all
// simply leave the
// field unset: none produces an error, none adds an omission reason,
// and none may fail the measurement. root is the repository root, so
// the file is read through filepath.Join(root, f.Path): the field is
// measured where the root-relative path lives, no matter where the run
// started.
func measureNoFinalNewline(root string, f *FileChange) {
	if f.Binary {
		return
	}
	info, err := os.Lstat(filepath.Join(root, f.Path))
	if err != nil {
		return
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return
	}
	file, err := os.Open(filepath.Join(root, f.Path))
	if err != nil {
		return
	}
	defer file.Close()
	if _, err := file.Seek(-1, io.SeekEnd); err != nil {
		return
	}
	var last [1]byte
	if _, err := io.ReadFull(file, last[:]); err != nil {
		return
	}
	if last[0] != '\n' {
		f.NoFinalNewline = true
	}
}

// fileStamp is the identity clue captured for one path that was
// already dirty when the run started: size, modification time, and the
// executable bit of the mode from Lstat, or an explicit absence for a
// staged or unstaged deletion, which is a legitimate dirty state. Size
// plus modification time is chosen because it reads no file contents
// and runs no hashing: it is exact where modification time has
// sub-second resolution (APFS, ext4, NTFS, the normal case), and it can
// miss a same-size rewrite inside one tick on a coarse-granularity
// filesystem (FAT, exFAT, some NFS mounts, older ext3). File contents
// are never read and git hash-object never runs. The executable bit is
// the one mode bit git tracks, so it belongs in the identity: a chmod
// between two non-executable modes does not register as a change.
type fileStamp struct {
	size    int64
	modTime time.Time
	exec    bool
	absent  bool
}

// snapshotDirtyStamps enumerates the paths already dirty in the
// workspace before the run starts and stamps each one, keyed by path,
// so the manifest can subtract the ones the run never moved.
// Enumerating with exactly the two commands the after pass already uses
// — git diff --name-only --ignore-submodules=all -z HEAD -- and git
// ls-files --others --exclude-standard -z — keeps both passes over the
// same universe; the diff ignores submodules so a dirty submodule is
// never stamped and never becomes a changed path, matching the
// measurement pass. The snapshot is anchored at the repository root,
// resolved once from dir, so every stamp key is root-relative and the
// after pass's keys agree with them whichever directory inside the
// repository the run was started from: stamp keys and measurement
// paths must name files the same way or the subtraction silently keys
// against nothing. Every command here is read-only with respect to
// the repository.
func snapshotDirtyStamps(ctx context.Context, dir string) (map[string]fileStamp, error) {
	root, err := repoRoot(ctx, dir)
	if err != nil {
		return nil, err
	}
	trackedOut, err := gitOutput(ctx, root, "diff", "--name-only", "--ignore-submodules=all", "-z", "HEAD", "--")
	if err != nil {
		return nil, fmt.Errorf("git diff --name-only: %w", err)
	}
	untrackedOut, err := gitOutput(ctx, root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	stamps := make(map[string]fileStamp)
	for _, path := range append(nulSplit(trackedOut), nulSplit(untrackedOut)...) {
		info, err := os.Lstat(filepath.Join(root, path))
		if err != nil {
			if os.IsNotExist(err) {
				stamps[path] = fileStamp{absent: true}
				continue
			}
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}
		stamps[path] = fileStamp{size: info.Size(), modTime: info.ModTime(), exec: info.Mode()&0o111 != 0}
	}
	return stamps, nil
}

// stampMatches reports whether the workspace path currently carries the
// captured pre-run stamp: both present with the same size, the same
// modification time, and the same executable bit, or absent then and
// absent now. A path that was not absent before but is a directory now
// has changed — a file replaced by a directory is not the same path it
// was. root is the repository root and path is root-relative, so a
// stamp taken in a run started inside a subdirectory still matches the
// file it names.
func stampMatches(root, path string, stamp fileStamp) (bool, error) {
	info, err := os.Lstat(filepath.Join(root, path))
	if err != nil {
		if os.IsNotExist(err) {
			return stamp.absent, nil
		}
		return false, err
	}
	if stamp.absent || info.IsDir() {
		return false, nil
	}
	return info.Size() == stamp.size && info.ModTime().Equal(stamp.modTime) && (info.Mode()&0o111 != 0) == stamp.exec, nil
}

// filePresent reports whether the root-relative path is currently a
// non-directory entry in the repository workspace, the ground truth for
// whether a file that existed at the base was deleted or merely
// emptied. root is the repository root, the same base every manifest
// path uses.
func filePresent(root, path string) (bool, error) {
	info, err := os.Lstat(filepath.Join(root, path))
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
// missing record is. root is the repository root and path is
// root-relative, so the command resolves the file it compares.
func measureUntrackedFile(ctx context.Context, root, path string) (added, deleted int, binary bool, err error) {
	out, err := gitOutputAnyExit(ctx, root, "diff", "--numstat", "--no-index", "-z", "--", "/dev/null", path)
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

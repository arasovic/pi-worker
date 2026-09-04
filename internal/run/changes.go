package run

import (
	"bytes"
	"context"
	"crypto/sha256"
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
// all. Paths that were already dirty before the run and whose identity
// did not move — size, modification time, executable bit, and the
// pre-run content hash for tracked paths, regular-file untracked
// paths, and symlinks — are subtracted, so the
// manifest answers what this run changed even on a dirty before-state.
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
	// Directory is true only when the changed path is itself a
	// directory: a collapsed untracked nested repository — a directory
	// carrying its own .git that git reports as one trailing-slash
	// entry in the untracked listing — that the run created or
	// removed. Git collapses a repository only while nothing about its
	// directory is known to the index; when git instead lists a
	// repository's inner files individually — a repository replacing a
	// tracked directory, or one that sits in a directory a tracked
	// file still claims — the manifest reports those files as ordinary
	// entries and no directory entry exists. The line counts are
	// always zero, since a directory has no lines to count, and
	// noFinalNewline is never present; status is added for a
	// repository the run created and deleted for one it removed. The
	// field is additive and optional, so schemaVersion stays 1.
	Directory bool `json:"directory,omitempty"`
	Binary    bool `json:"binary,omitempty"`
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
// before-state HEAD, subtracting the before-dirty paths whose identity
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
func measureChanges(ctx context.Context, dir string, before *GitState, dirtyStamps map[string]fileStamp, metadata *gitMetadataSnapshot) *Changes {
	if before.Head == "" {
		return &Changes{Omitted: reasonUnbornHead}
	}
	root, err := repoRoot(ctx, dir)
	if err != nil {
		return &Changes{Omitted: reasonMeasurementFail}
	}
	if metadata != nil {
		// Unsafe pre-existing trust state makes the measurement
		// unavailable even when nothing drifted during the run: an index
		// visibility marker that was already set hides a rewrite with no
		// post-run metadata drift to detect, an effective
		// core.trustctime of false lets git trust the stat cache over
		// content, so git itself cannot see a same-size rewrite with a
		// restored modification time, and core.fileMode=false makes git
		// suppress every mode-only tracked difference, so a chmod the
		// run made on an untouched file is invisible to every diff the
		// measurement runs. A confident zero under any of these would
		// be a promise git's own trust settings do not support, so the
		// manifest states the measurement-failed reason instead.
		if metadata.unsafeIndex || !metadata.trustCtime || !metadata.fileMode {
			return &Changes{Omitted: reasonMeasurementFail}
		}
		drifted, err := snapshotGitMetadataAt(ctx, root, metadata)
		if err != nil || drifted {
			return &Changes{Omitted: reasonMeasurementFail}
		}
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
// command per file up to the cap; a collapsed untracked nested
// repository — a directory with its own .git, reported by git as one
// directory entry without entering it — is kept as one directory entry
// and never diffed, because no-index cannot compare a directory against
// /dev/null and a single unmeasurable directory must not sink the whole
// manifest. Every command is read-only: no add
// (including intent-to-add), stash, commit, checkout, reset, clean, or
// branch, tag, or worktree operation. Any parse failure or command
// failure fails the whole measurement: an approximate manifest
// presented as truth is worse than none.
//
// The listing shapes the manifest must handle are bounded and exact,
// and they are git's own shapes, taken as git reports them. Git
// collapses an untracked nested repository to one directory entry only
// while nothing about the directory is known to the index: a fresh
// repository at a path no tracked file ever occupied, or one added
// inside a tracked directory whose own files survive, lists as one
// trailing-slash entry and nothing inside it is listed. The collapse
// does not survive a tracked-path replacement: when a run deletes the
// tracked files of a directory and leaves a nested repository at that
// same directory — or a nested repository sits in a directory where a
// tracked file still lives — git descends into the repository and lists
// its files one by one alongside the tracked deletions or
// modifications, because the directory is known to the index. And a
// nested repository that replaces a tracked file of the same name is
// not untracked at all: git reports the path as a tracked modification
// and lists none of the repository's contents. The manifest reports
// each shape as git reports it: one collapsed directory entry when git
// collapses, one ordinary entry per listed file when git lists, and a
// plain tracked entry when git treats the replacement as a tracked
// change. A nested repository never disables the manifest: no
// directory is ever diffed, every listed entry is a file the no-index
// measurement can compare, and where the contents of a pre-existing
// collapsed repository are invisible to the superproject's own listing
// they are invisible here too — the manifest never claims more than
// git itself can see.
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
	untrackedRaw := nulSplit(untrackedOut)
	// Git collapses an untracked nested repository — a directory with
	// its own .git, never a gitlink submodule — to one directory entry
	// with a trailing slash, without entering it. Those entries are the
	// only directories the listing carries; an ordinary untracked
	// directory is listed by its files, and a symlink is listed as the
	// link itself. Every path is canonicalised by dropping that single
	// trailing slash, because the same root-relative path is the same
	// path whether it names a directory or a file: a path that was a
	// file when the dirty snapshot ran and a nested repository when the
	// measurement runs must merge into one entry, never two spellings
	// of one path counted twice. The listing is git's own, taken as it
	// is: git collapses the repository only while nothing about its
	// directory is known to the index. A repository that replaces a
	// tracked directory — or sits beside a tracked file inside the same
	// directory — is listed by git file by file instead of collapsing, and
	// one that replaces a tracked file of the same name is reported as a
	// tracked modification with no untracked listing at all. Inner files
	// git lists are then measured exactly like every other listed file;
	// the manifest makes no attempt to force a collapsed entry where git
	// itself does not report one.
	untracked := make([]string, 0, len(untrackedRaw))
	untrackedDirs := make(map[string]bool, len(untrackedRaw))
	for _, path := range untrackedRaw {
		if strings.HasSuffix(path, "/") {
			path = strings.TrimSuffix(path, "/")
			untrackedDirs[path] = true
		}
		untracked = append(untracked, path)
	}

	// The candidate set is the union of three sets: the tracked diff
	// against the before-state HEAD, the untracked listing, and the
	// paths that were dirty before the run. From that union, every
	// before-dirty path whose identity is unchanged — same size, same
	// modification time, same executable bit, and for tracked paths,
	// regular-file untracked paths, and symlinks the same pre-run
	// content hash, or absent then and absent now — is
	// subtracted: it was equally dirty before the run and the run
	// contributed nothing to it, so it names no change this run made.
	// The content hash is what keeps a same-size rewrite with a
	// restored modification time out of the subtraction: its stat
	// matches by construction, and only the bytes can say it moved. A
	// collapsed nested repository that was already there when the run
	// started is unchanged when the run left it a collapsed entry:
	// directory presence in the listing is the only fact the workspace
	// level can measure for a collapsed repository, and the contents
	// inside are the embedded repository's own business, never
	// inspected and never allowed to move the directory in or out of
	// the manifest. A directory stamp whose path is no longer a
	// collapsed entry in the after listing is not unchanged — the
	// repository was removed or its tracked containment changed so git
	// lists its files individually, and the after state must be
	// reported rather than silently swallowed. The dirty
	// union term is not optional: without it a worker that reverts an
	// already-dirty file to its committed content drops out of the HEAD
	// diff completely and the manifest would go silent even though the
	// caller's uncommitted work was destroyed.
	unchanged := make(map[string]bool, len(dirtyStamps))
	for path, stamp := range dirtyStamps {
		if stamp.dir {
			continue
		}
		matches, err := stampMatches(root, path, stamp)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}
		if matches {
			unchanged[path] = true
		}
	}
	for path, stamp := range dirtyStamps {
		if !stamp.dir {
			continue
		}
		if stamp.descendants == nil {
			// A pre-existing collapsed repository left alone stays a
			// collapsed entry in the after listing and is subtracted.
			// The only identity a collapsed directory carries is its
			// presence in that listing; anything the run changed about
			// its shape — removed the repository, or moved tracked
			// files in or out of its directory so git stops collapsing
			// it and lists the inner files individually — drops out of
			// the collapsed listing and must be reported through the
			// entries git now lists, never silently subtracted.
			if untrackedDirs[path] {
				unchanged[path] = true
			}
			continue
		}
		info, err := os.Lstat(filepath.Join(root, path))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}
		if !info.IsDir() {
			continue
		}
		// The marker distinguishes an empty ordinary directory from a
		// nested repository even when both list no descendants.
		currentMarker, err := gitMarkerPresent(root, path)
		if err != nil {
			return nil, fmt.Errorf("stat %s/.git: %w", path, err)
		}
		if currentMarker != stamp.gitMarker {
			continue
		}
		current := visibleDescendantPaths(path, untracked)
		if !stringSlicesEqual(current, stamp.descendants) {
			continue
		}
		allUnchanged := true
		for _, descendant := range current {
			if !unchanged[descendant] {
				allUnchanged = false
				break
			}
		}
		if allUnchanged {
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
		// A collapsed nested repository is a real, stable workspace
		// change at the directory's own path, but it is not a file that
		// --no-index can compare against /dev/null: Git tries to open a
		// path beneath /dev/null and fails, and failing the whole
		// measurement over one unmeasurable directory is exactly the
		// bug being prevented. Report the directory as one added entry
		// marked directory, with zero line counts, without entering its
		// repository or claiming ownership of any file inside it. Only
		// this collapsed shape reaches the directory branch: git lists
		// the contents of a nested repository individually whenever the
		// repository replaces a tracked directory or sits in a
		// directory a tracked file still claims — only a repository at
		// a path unknown to the index is collapsed — so every other
		// nested-repository change is reported as the ordinary
		// per-file entries git itself lists, honestly counted.
		info, err := os.Lstat(filepath.Join(root, path))
		if err != nil {
			return nil, fmt.Errorf("stat untracked path %s: %w", path, err)
		}
		if info.IsDir() {
			appendFile(FileChange{Path: path, Status: statusAdded, Directory: true})
			continue
		}
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
	// counts and dirtyBefore true; a directory stamp keeps its directory
	// marker, so removing a pre-existing nested repository is reported
	// as a deleted directory entry, not as a deleted file. Current
	// presence decides first: a
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
	for path, stamp := range dirtyStamps {
		if unchanged[path] || inTracked[path] || inUntracked[path] {
			continue
		}
		if _, seen := byPath[path]; seen {
			continue
		}
		f := FileChange{Path: path, DirtyBefore: true, Directory: stamp.dir}
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
	// behaves. This is the third place the manifest reads file content:
	// the pre-run dirty snapshot hashes tracked paths, regular-file
	// untracked paths, and symlinks up front for the subtraction
	// identity, while this field reads one byte per listed file here —
	// each read deliberate and bounded, never the whole file pulled
	// into memory.
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
// already dirty when the run started: size, modification time, the
// executable bit of the mode from Lstat, and the SHA-256 of the path's
// content — for tracked paths, regular-file untracked paths, and
// symlinks — or an explicit absence for a staged or unstaged deletion,
// which is a legitimate dirty state. A path that was a directory when
// the snapshot ran — a collapsed untracked nested repository, the only
// directory the dirty listing can carry — gets a directory stamp
// instead: size, modification time and the executable bit describe a
// file's contents, and a directory's contents are the embedded
// repository's business. For a collapsed directory, presence in the
// after listing is the only identity the workspace level can measure;
// when the run removes the repository or a tracked file lands inside
// it, git stops collapsing the directory and the after state is
// reported through the files git lists at the path instead. Size plus
// modification time is exact where modification time has sub-second
// resolution (APFS, ext4, NTFS, the normal case), and it can miss a
// same-size rewrite inside one tick on a coarse-granularity filesystem
// (FAT, exFAT, some NFS mounts, older ext3); the content hash is what
// keeps a deliberate same-size rewrite with a restored modification
// time from being subtracted as untouched — git's own diff would still
// see the rewrite through the ctime, but the stamp must not depend on
// which of git's stat fields the repository still trusts. The hash
// belongs to every dirty path whose content the subtraction must tell
// "never moved" from "rewritten invisibly" — a tracked path, which
// git's stat cache can hide, and an untracked regular file or symlink,
// whose pre-run content the subtraction must likewise not lose. A
// regular file is hashed by its bytes and a symlink by its target
// string, the same content git would compare; the bytes are read once
// up front, reduced to 32 bytes, and never retained, reported, or
// transmitted. The executable bit is the one mode bit git tracks, so
// it belongs in the identity: a chmod between two non-executable modes
// does not register as a change.
type fileStamp struct {
	size    int64
	modTime time.Time
	exec    bool
	absent  bool
	// dir is true when the stamped path is a directory, not a file.
	dir bool
	// gitMarker is true when the directory had a .git entry at the
	// snapshot: it distinguishes an ordinary directory from a nested
	// repository without entering it.
	gitMarker bool
	// descendants is the sorted visible untracked descendant set for a
	// tracked-path directory stamp; nil marks the collapsed-repository
	// shape instead.
	descendants []string
	// contentHash is the identity of the path's content at snapshot
	// time, captured for tracked paths, regular-file untracked paths,
	// and symlinks; hashed reports whether it was captured. An absent
	// path, an entry kind whose content identity is not defined (not a
	// regular file, not a symlink), and a path the stamping chose not
	// to read carry hashed false and are compared by their stat stamp
	// alone.
	contentHash [sha256.Size]byte
	hashed      bool
}

// gitMetadataSnapshot is the read-only pre-run record of the Git trust
// inputs that can make the post-run path queries lie. Five families of
// inputs are captured, each from the actual repository rather than from
// a hard-coded patch:
//
//   - Index visibility markers. ls-files -v marks every entry whose
//     comparison git may suppress: skip-worktree (S) and the lowercase
//     assume-unchanged class. A marker present before the run makes the
//     normal tracked diff untrustworthy even when nothing changed during
//     the run, because a rewrite of that entry is invisible both before
//     and after; a marker that appears during the run is drift. Ordinary
//     index changes from a worker's add or commit never produce markers,
//     so they never make an otherwise trustworthy manifest unavailable.
//   - The effective core.trustctime value. Git trusts the stat cache
//     without re-reading content when size and modification time match
//     and ctime is not trusted; a pre-existing false value makes any
//     same-size restored-mtime rewrite invisible to every git command
//     the measurement runs, and a value that changed during the run is
//     drift. Unset means true, git's default.
//   - The effective core.fileMode value. When it is false, git
//     suppresses every tracked mode-only difference: a chmod a worker
//     makes on an untouched file would be invisible to every diff the
//     measurement runs, so the pre-existing value gates the
//     measurement and a value that changed during the run is drift.
//     Unset means true, git's default. (This feature does not implement
//     core.ignoreStat; only the two mode/stat trust values git reads
//     as booleans are watched.)
//   - The ignore-rule files git consults beyond the tree: the effective
//     repository info/exclude path and the effective core.excludesFile
//     file (the configured value, or the XDG default when unset). Each
//     is stamped without reading its contents; a change during the run
//     can hide untracked paths from the exclude-standard listing, so if
//     a file's metadata moves, or the effective value changes, the
//     measurement is unavailable rather than a confident zero.
//   - The in-tree .gitignore rule files, enumerated with git's own
//     listings rather than a filesystem walk: the tracked ones, the
//     visible untracked ones, and the ones git itself ignores — a
//     rule file whose own rules exclude it still applies to the paths
//     beside it, and a newly created self-ignored rule file would
//     otherwise hide both itself and its payload from every listing.
//     Each file's set membership and content identity are captured; a
//     rule file that appears, disappears, or changes content during the
//     run can hide untracked paths the run wrote, so any of those makes
//     the measurement unavailable. Rule files that already existed when
//     the run started applied equally to both passes, so an unchanged
//     pre-existing rule file never trips the watch. This family reads
//     the content of the rule files it names — they are workspace
//     files, small, and their rules are exactly the trust input under
//     test — unlike the beyond-tree exclude files, which stay
//     metadata-only because they live outside the workspace.
//
// A git command failure while capturing any input fails the run's
// measurement the same way a later command failure does: never a silent
// snapshot that trusts less than it recorded.
type gitMetadataSnapshot struct {
	unsafeIndex bool
	trustCtime  bool
	// fileMode is the effective core.fileMode value, defaulting to true
	// when the key is unset: a false value makes git suppress every
	// tracked mode-only difference, so a chmod the run made on an
	// untouched file would be invisible to the post-run diff.
	fileMode bool
	// excludePath and exclude name and stamp the repository's
	// info/exclude file, resolved once at snapshot time.
	excludePath string
	exclude     metadataStamp
	// excludesFile is the effective core.excludesFile value, empty when
	// unset; excludesFilePath is that value resolved to the file whose
	// rules apply (the configured path, or the XDG default when unset,
	// or empty when the default cannot be resolved); excludes stamps it.
	excludesFile     string
	excludesFilePath string
	excludes         metadataStamp
	// treeRules maps each in-tree .gitignore rule file git's own
	// listings name — tracked, visible untracked, or itself ignored —
	// to its content identity at snapshot time. A rule file that
	// appears, disappears, or changes content during the run can hide
	// untracked paths from the exclude-standard listing, so the set and
	// the identities are compared after the run; the map is empty when
	// the repository carries no .gitignore files at all.
	treeRules map[string]gitignoreRuleStamp
}

// metadataStamp is deliberately metadata-only. In particular, this does
// not hash or copy an exclude file: the measurement boundary must not pull
// user file contents out of the workspace.
type metadataStamp struct {
	size    int64
	modTime time.Time
	mode    os.FileMode
	absent  bool
}

// snapshotGitMetadata records the index visibility markers, the effective
// core.trustctime and core.fileMode values, the effective
// ignore-rule files beyond the tree, and the in-tree .gitignore rule
// files before workers start. Every operation is a read-only Git query
// or an Lstat, plus a content read of the .gitignore rule files
// themselves; no index refresh or object write is involved.
func snapshotGitMetadata(ctx context.Context, root string) (*gitMetadataSnapshot, error) {
	unsafeIndex, err := readIndexMetadata(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("git ls-files metadata: %w", err)
	}
	trustCtime, err := readTrustCtime(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("git config core.trustctime: %w", err)
	}
	fileMode, err := readFileMode(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("git config core.fileMode: %w", err)
	}
	excludePath, err := gitPath(ctx, root, "info/exclude")
	if err != nil {
		return nil, err
	}
	exclude, err := statMetadata(excludePath)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", excludePath, err)
	}
	excludesFile, err := gitConfigValue(ctx, root, "core.excludesFile")
	if err != nil {
		return nil, fmt.Errorf("git config core.excludesFile: %w", err)
	}
	excludesFilePath := resolveExcludesFile(root, excludesFile)
	excludes := metadataStamp{}
	if excludesFilePath != "" {
		if excludes, err = statMetadata(excludesFilePath); err != nil {
			return nil, fmt.Errorf("stat %s: %w", excludesFilePath, err)
		}
	}
	treeRules, err := snapshotTreeGitignoreRules(ctx, root)
	if err != nil {
		return nil, err
	}
	return &gitMetadataSnapshot{
		unsafeIndex:      unsafeIndex,
		trustCtime:       trustCtime,
		fileMode:         fileMode,
		excludePath:      excludePath,
		exclude:          exclude,
		excludesFile:     excludesFile,
		excludesFilePath: excludesFilePath,
		excludes:         excludes,
		treeRules:        treeRules,
	}, nil
}

// readIndexMetadata reports whether any ls-files record carries an index
// visibility bit that Git may use to suppress a worktree comparison.
// Uppercase S means skip-worktree; lowercase markers mean
// assume-unchanged. It does not retain paths or contents, so ordinary
// index changes from a worker's add or commit do not make an otherwise
// trustworthy manifest unavailable.
func readIndexMetadata(ctx context.Context, root string) (bool, error) {
	out, err := gitOutput(ctx, root, "ls-files", "-v", "-z", "--")
	if err != nil {
		return false, err
	}
	for _, record := range strings.Split(out, "\x00") {
		if record == "" {
			continue
		}
		if len(record) < 3 || record[1] != ' ' || record[2:] == "" {
			return false, fmt.Errorf("malformed ls-files record %q", record)
		}
		if record[0] == 'S' || (record[0] >= 'a' && record[0] <= 'z') {
			return true, nil
		}
	}
	return false, nil
}

// readTrustCtime reports the effective core.trustctime value: false when
// the repository configuration says false, true when it says true, and
// true when the key is unset, which is git's default. An unreadable or
// otherwise failing configuration is an error — a measurement must never
// guess which trust value git will act under. --bool makes git itself
// normalize every accepted spelling, so the comparison is over git's own
// words, not over the raw configuration text.
func readTrustCtime(ctx context.Context, root string) (bool, error) {
	value, err := gitConfigValue(ctx, root, "--bool", "core.trustctime")
	if err != nil {
		return false, err
	}
	switch value {
	case "":
		return true, nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, fmt.Errorf("unexpected core.trustctime value %q", value)
}

// readFileMode reports the effective core.fileMode value: false when
// the repository configuration says false, true when it says true, and
// true when the key is unset, which is git's default. When the value is
// false, git suppresses every tracked mode-only difference, so a chmod
// the run made on an untouched file would be invisible to the diff the
// measurement runs; the effective value therefore gates the
// measurement, and a value that changed during the run is drift. An
// unreadable or otherwise failing configuration is an error — a
// measurement must never guess which trust value git will act under.
// --bool makes git itself normalize every accepted spelling, so the
// comparison is over git's own words, not over the raw configuration
// text.
func readFileMode(ctx context.Context, root string) (bool, error) {
	value, err := gitConfigValue(ctx, root, "--bool", "core.fileMode")
	if err != nil {
		return false, err
	}
	switch value {
	case "":
		return true, nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, fmt.Errorf("unexpected core.fileMode value %q", value)
}

// gitConfigValue reads one configuration value from the repository's
// effective configuration, returning "" when the key is unset (git
// exits 1 with empty output for a missing key). Any other failure is an
// error. args may carry the leading --bool or --path modifiers the same
// way a git command line would.
func gitConfigValue(ctx context.Context, root string, args ...string) (string, error) {
	full := append([]string{"config", "--get"}, args...)
	out, err := gitOutput(ctx, root, full...)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && strings.TrimSpace(out) == "" {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// gitPath resolves one repository-relative path the way git itself
// resolves it: rev-parse --git-path prints a path relative to the
// command's working directory (normally .git/info/exclude), so it is
// resolved against the same root used for every other measurement path.
func gitPath(ctx context.Context, root, name string) (string, error) {
	out, err := gitOutput(ctx, root, "rev-parse", "--git-path", name)
	if err != nil {
		return "", fmt.Errorf("git rev-parse --git-path %s: %w", name, err)
	}
	path := strings.TrimSpace(out)
	if path == "" {
		return "", fmt.Errorf("git rev-parse --git-path %s: empty path", name)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	return path, nil
}

// resolveExcludesFile resolves the effective core.excludesFile value to
// the file whose rules git will consult: the value itself (with a
// leading ~ expanded against HOME, a relative value resolved against
// the repository root, the working directory of every measurement
// command) or, when the value is empty, git's XDG default
// $XDG_CONFIG_HOME/git/ignore with ~/.config/git/ignore as its own
// default. It returns "" only when no file can be named — HOME and
// XDG_CONFIG_HOME both missing — in which case the value comparison
// still guards the input and there is simply no file to stamp.
func resolveExcludesFile(root, value string) string {
	if value != "" {
		if strings.HasPrefix(value, "~/") {
			if home := os.Getenv("HOME"); home != "" {
				return filepath.Join(home, value[2:])
			}
			return ""
		}
		if filepath.IsAbs(value) {
			return filepath.Clean(value)
		}
		return filepath.Join(root, value)
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "git", "ignore")
	}
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".config", "git", "ignore")
	}
	return ""
}

// statMetadata stamps a Git metadata file without reading it. A missing
// file is a meaningful state: creating info/exclude during a run is drift.
func statMetadata(path string) (metadataStamp, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return metadataStamp{absent: true}, nil
		}
		return metadataStamp{}, err
	}
	return metadataStamp{size: info.Size(), modTime: info.ModTime(), mode: info.Mode()}, nil
}

func metadataStampEqual(a, b metadataStamp) bool {
	return a.size == b.size && a.modTime.Equal(b.modTime) && a.mode == b.mode && a.absent == b.absent
}

// gitignoreRuleStamp is the content identity of one in-tree .gitignore
// rule file at snapshot time: whether the file is absent, and the
// SHA-256 of its content when present — a regular file's bytes or a
// symlink's target string, the same content identity the dirty-path
// stamping captures. The rules a .gitignore carries are its content, so
// content identity is the precise ground truth for whether the rules
// moved during the run: a same-size rewrite with a restored modification
// time changes the rules and must be drift, while a write that leaves
// the bytes identical — a touched file whose rules did not change —
// must not make the measurement unavailable. An entry kind whose
// content this identity does not define (a directory, which a
// .gitignore never legitimately is) carries hashed false and can never
// equal a hashed before-stamp, so a kind change is drift.
type gitignoreRuleStamp struct {
	absent      bool
	contentHash [sha256.Size]byte
	hashed      bool
}

func gitignoreRuleStampEqual(a, b gitignoreRuleStamp) bool {
	if a.absent != b.absent || a.hashed != b.hashed {
		return false
	}
	return a.absent || a.contentHash == b.contentHash
}

// snapshotTreeGitignoreRules enumerates the in-tree .gitignore rule
// files with git's own listing commands, never with a filesystem walk
// and never by entering a nested repository: git ls-files already
// collapses an untracked nested repository to one directory entry and
// never descends into it, so no .gitignore inside one is listed, and a
// nested repository the outer git lists file by file contributes only
// the entries git itself names. Three listings cover every rule file
// git consults: the tracked ones (ls-files without --others, which
// lists from the index and never consults ignore rules), the visible
// untracked ones (--others --exclude-standard), and the ones git
// itself ignores (--others --ignored --exclude-standard) — a rule
// file whose own rules exclude it still applies to the paths beside
// it, and a self-ignored rule file is invisible to the plain
// exclude-standard listing. The pathspec (:(glob)**/.gitignore)
// prunes each listing to the rule-file names, so the cost stays
// proportional to the .gitignore files in the tree rather than to
// every ignored file in it. Each listed file is stamped with its
// content identity; a listed file that has disappeared is an absent
// stamp, and a git command failure or an unreadable rule file is an
// error — never a silent snapshot that trusts less than it recorded.
func snapshotTreeGitignoreRules(ctx context.Context, root string) (map[string]gitignoreRuleStamp, error) {
	paths := make(map[string]bool)
	listings := [][]string{
		{"ls-files", "-z", "--", ":(glob)**/.gitignore"},
		{"ls-files", "--others", "--exclude-standard", "-z", "--", ":(glob)**/.gitignore"},
		{"ls-files", "--others", "--ignored", "--exclude-standard", "-z", "--", ":(glob)**/.gitignore"},
	}
	for _, args := range listings {
		out, err := gitOutput(ctx, root, args...)
		if err != nil {
			return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		for _, path := range nulSplit(out) {
			paths[path] = true
		}
	}
	rules := make(map[string]gitignoreRuleStamp, len(paths))
	for path := range paths {
		info, err := os.Lstat(filepath.Join(root, path))
		if err != nil {
			if os.IsNotExist(err) {
				rules[path] = gitignoreRuleStamp{absent: true}
				continue
			}
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}
		hash, hashed, err := hashPathContent(root, path, info)
		if err != nil {
			return nil, err
		}
		rules[path] = gitignoreRuleStamp{contentHash: hash, hashed: hashed}
	}
	return rules, nil
}

// gitignoreRuleSetsEqual reports whether two snapshots of the in-tree
// .gitignore rule files name the same set of paths with the same
// content identity at every path. A rule file that appeared or
// disappeared during the run, or whose content changed, is drift: any
// of them can hide untracked paths the run wrote behind rules the
// pre-run listing never applied.
func gitignoreRuleSetsEqual(a, b map[string]gitignoreRuleStamp) bool {
	if len(a) != len(b) {
		return false
	}
	for path, stamp := range a {
		after, ok := b[path]
		if !ok || !gitignoreRuleStampEqual(stamp, after) {
			return false
		}
	}
	return true
}

// snapshotGitMetadataAt re-reads the post-run trust inputs from the same
// root and the paths selected before the run, and reports whether any of
// them drifted. Keeping the paths from the before pass avoids trusting a
// worker-modified Git configuration to identify the files; the three
// effective values are re-read fresh, because a worker can redirect
// them to different files. Each family is compared independently, so a
// single drift is enough to make the measurement unavailable: one wrong
// "clean" is worse than an admitted "unavailable".
func snapshotGitMetadataAt(ctx context.Context, root string, before *gitMetadataSnapshot) (bool, error) {
	unsafeIndex, err := readIndexMetadata(ctx, root)
	if err != nil {
		return false, err
	}
	if !before.unsafeIndex && unsafeIndex {
		return true, nil
	}
	trustCtime, err := readTrustCtime(ctx, root)
	if err != nil {
		return false, err
	}
	if trustCtime != before.trustCtime {
		return true, nil
	}
	fileMode, err := readFileMode(ctx, root)
	if err != nil {
		return false, err
	}
	if fileMode != before.fileMode {
		return true, nil
	}
	afterExclude, err := statMetadata(before.excludePath)
	if err != nil {
		return false, err
	}
	if !metadataStampEqual(before.exclude, afterExclude) {
		return true, nil
	}
	excludesFile, err := gitConfigValue(ctx, root, "core.excludesFile")
	if err != nil {
		return false, err
	}
	if excludesFile != before.excludesFile {
		return true, nil
	}
	if before.excludesFilePath != "" {
		afterExcludes, err := statMetadata(before.excludesFilePath)
		if err != nil {
			return false, err
		}
		if !metadataStampEqual(before.excludes, afterExcludes) {
			return true, nil
		}
	}
	afterTreeRules, err := snapshotTreeGitignoreRules(ctx, root)
	if err != nil {
		return false, err
	}
	if !gitignoreRuleSetsEqual(before.treeRules, afterTreeRules) {
		return true, nil
	}
	return false, nil
}

// snapshotDirtyStamps enumerates the paths already dirty in the
// workspace before the run starts and stamps each one, keyed by path,
// so the manifest can subtract the ones the run never moved.
// Enumerating with exactly the two commands the after pass already uses
// — git diff --name-only --ignore-submodules=all -z HEAD -- and git
// ls-files --others --exclude-standard -z — keeps both passes over the
// same universe; the diff ignores submodules so a dirty submodule is
// never stamped and never becomes a changed path, matching the
// measurement pass. Both listings are git's own, so a nested repository
// that git collapses to one directory entry is stamped as that
// collapsed directory, and one that git descends into because a
// tracked file still lives inside it is stamped file by file, its
// inner contents visible to the run and therefore to the manifest. The
// snapshot is anchored at the repository root,
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
	trackedRaw := nulSplit(trackedOut)
	trackedPaths := make(map[string]bool, len(trackedRaw))
	for _, path := range trackedRaw {
		trackedPaths[strings.TrimSuffix(path, "/")] = true
	}
	untrackedOut, err := gitOutput(ctx, root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	untrackedRaw := nulSplit(untrackedOut)
	untrackedDirs := make(map[string]bool, len(untrackedRaw))
	for _, path := range untrackedRaw {
		if strings.HasSuffix(path, "/") {
			untrackedDirs[strings.TrimSuffix(path, "/")] = true
		}
	}
	stamps := make(map[string]fileStamp, len(trackedRaw)+len(untrackedRaw))
	// Tracked paths carry the content identity: their stat stamp alone
	// would match a deliberate same-size rewrite with a restored
	// modification time, and the subtraction must not rest on a stat
	// comparison the writer can forge — only a hash of the pre-run
	// content can tell "never moved" from "rewritten invisibly". A
	// pre-existing untracked regular file or symlink gets the same
	// content identity, by the same reasoning and at the same price: a
	// worker can rewrite it in place with identical size and a restored
	// modification time, and only the bytes — or the link's target
	// string, which is its content — can say it moved. A collapsed
	// untracked nested repository is the one shape that cannot carry
	// this identity: git reports the whole directory as one listing
	// entry without entering it, its contents are the embedded
	// repository's business, and presence in the listing is the only
	// identity the workspace level can measure, so its stamp is a bare
	// directory marker.
	for _, raw := range append(trackedRaw, untrackedRaw...) {
		// Stamp keys are canonicalised exactly like the measurement
		// pass canonicalises its listing: drop the trailing slash git
		// appends to a collapsed nested-repository entry, so a key and
		// a measurement path agree byte for byte and the subtraction
		// never silently keys against nothing.
		path := strings.TrimSuffix(raw, "/")
		info, err := os.Lstat(filepath.Join(root, path))
		if err != nil {
			if os.IsNotExist(err) {
				stamps[path] = fileStamp{absent: true}
				continue
			}
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}
		if info.IsDir() {
			stamp := fileStamp{dir: true}
			gitMarker, err := gitMarkerPresent(root, path)
			if err != nil {
				return nil, fmt.Errorf("stat %s/.git: %w", path, err)
			}
			stamp.gitMarker = gitMarker
			if trackedPaths[path] && !untrackedDirs[path] {
				stamp.descendants = visibleDescendantPaths(path, untrackedRaw)
			}
			stamps[path] = stamp
			continue
		}
		stamp, err := snapshotStamp(root, path, true)
		if err != nil {
			return nil, err
		}
		stamps[path] = stamp
	}
	return stamps, nil
}

// gitMarkerPresent reports whether path/.git exists, without
// following symlinks.
func gitMarkerPresent(root, path string) (bool, error) {
	_, err := os.Lstat(filepath.Join(root, path, ".git"))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func visibleDescendantPaths(path string, names []string) []string {
	prefix := path + "/"
	descendants := make([]string, 0)
	for _, name := range names {
		name = strings.TrimSuffix(name, "/")
		if strings.HasPrefix(name, prefix) {
			descendants = append(descendants, name)
		}
	}
	sort.Strings(descendants)
	return descendants
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// snapshotStamp builds the pre-run stamp of one dirty path. With
// captureHash true the stamp also carries the path's content identity,
// read once and reduced to a hash: a regular file's bytes and a
// symlink's target string — the content git itself would compare —
// while any other entry kind is stamped without a hash rather than
// guessed at; a read failure is an error, never a silent stat-only
// fallback: the manifest must not subtract a path whose pre-run
// identity it failed to capture. root is the repository root and path
// is root-relative, the same coordinates every stamp and measurement
// path uses.
func snapshotStamp(root, path string, captureHash bool) (fileStamp, error) {
	info, err := os.Lstat(filepath.Join(root, path))
	if err != nil {
		if os.IsNotExist(err) {
			return fileStamp{absent: true}, nil
		}
		return fileStamp{}, fmt.Errorf("stat %s: %w", path, err)
	}
	stamp := fileStamp{size: info.Size(), modTime: info.ModTime(), exec: info.Mode()&0o111 != 0}
	if !captureHash {
		return stamp, nil
	}
	hash, hashed, err := hashPathContent(root, path, info)
	if err != nil {
		return fileStamp{}, err
	}
	stamp.contentHash = hash
	stamp.hashed = hashed
	return stamp, nil
}

// hashPathContent computes the content identity of the workspace path:
// the SHA-256 of a regular file's bytes, read in a stream so a
// multi-gigabyte file never enters memory whole, or of a symlink's
// target string from Readlink, which never follows the link. hashed is
// false only for an entry kind whose content this function does not
// define (not a regular file, not a symlink), so the caller can keep a
// stat-only stamp for it; a failure to read a kind it does define is an
// error. The content is reduced to 32 bytes and nothing else is ever
// done with it: the manifest reports hashes to no one, and the bytes
// themselves are never retained.
func hashPathContent(root, path string, info os.FileInfo) ([sha256.Size]byte, bool, error) {
	switch {
	case info.Mode().IsRegular():
		file, err := os.Open(filepath.Join(root, path))
		if err != nil {
			return [sha256.Size]byte{}, false, fmt.Errorf("open %s: %w", path, err)
		}
		defer file.Close()
		digest := sha256.New()
		if _, err := io.Copy(digest, file); err != nil {
			return [sha256.Size]byte{}, false, fmt.Errorf("read %s: %w", path, err)
		}
		var hash [sha256.Size]byte
		copy(hash[:], digest.Sum(nil))
		return hash, true, nil
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(filepath.Join(root, path))
		if err != nil {
			return [sha256.Size]byte{}, false, fmt.Errorf("readlink %s: %w", path, err)
		}
		return sha256.Sum256([]byte(target)), true, nil
	default:
		return [sha256.Size]byte{}, false, nil
	}
}

// contentMatchesHash reports whether the path's current content carries
// the captured identity: a regular file's bytes and a symlink's target
// string, hashed the same way the pre-run stamp hashed them. An entry
// whose kind changed since the snapshot — or that is neither a regular
// file nor a symlink now — cannot match: kind is part of the identity,
// and a changed kind is a change the manifest must report. A read
// failure is an error, never a match: the subtraction must not rest on
// an identity it could not verify.
func contentMatchesHash(root, path string, info os.FileInfo, want [sha256.Size]byte) (bool, error) {
	hash, hashed, err := hashPathContent(root, path, info)
	if err != nil {
		return false, err
	}
	return hashed && hash == want, nil
}

// stampMatches reports whether the workspace path currently carries the
// captured pre-run identity: both present with the same size, the same
// modification time, and the same executable bit — plus, when the
// pre-run stamp captured a content hash, the same content, which is
// the identity captured for tracked paths, regular-file untracked
// paths, and symlinks. Absent then and absent now also matches. The
// content check is what keeps a same-size rewrite with a restored
// modification time from being subtracted as untouched: the stat
// fields match by construction, and only the hash can see that the
// bytes are not the ones the run started with, or that a link's target
// string is not the one the run started with. A path that was not
// absent before but is a directory now has changed — a file replaced
// by a directory is not the same path it was. root is the repository
// root and path is root-relative, so a stamp taken in a run started
// inside a subdirectory still matches the file it names.
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
	if info.Size() != stamp.size || !info.ModTime().Equal(stamp.modTime) || (info.Mode()&0o111 != 0) != stamp.exec {
		return false, nil
	}
	if !stamp.hashed {
		return true, nil
	}
	return contentMatchesHash(root, path, info, stamp.contentHash)
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

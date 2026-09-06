// Package background defines the snapshot carried across run
// lifecycle phases — the accepted-state record written before workers
// start, describing tasks, workers, workspace and supervisor identity.
package background

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/arasovic/pi-worker/internal/config"
	"github.com/arasovic/pi-worker/internal/runlog"
)

// snapshotWriteFile is the minimal subset of *os.File used when creating
// snapshot files. It allows tests to substitute fake files without changing
// call sites.
type snapshotWriteFile interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
	Chmod(fs.FileMode) error
	Name() string
}

// openSnapshotForCreate opens the snapshot path exclusively for writing
// with owner-only permissions.
var openSnapshotForCreate = func(path string) (snapshotWriteFile, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
}

// createSnapshotTemp creates a temporary file in dir with pattern and
// owner-only permissions.
var createSnapshotTemp = func(dir, pattern string) (snapshotWriteFile, error) {
	return os.CreateTemp(dir, pattern)
}

// renameSnapshot atomically renames a file, replacing the destination.
var renameSnapshot = os.Rename

// removeSnapshotForCleanup removes a file during cleanup paths.
var removeSnapshotForCleanup = os.Remove

// snapshotPath returns <root>/<runId>/snapshot.json.
func snapshotPath(root, runID string) string {
	return filepath.Join(root, runID, "snapshot.json")
}

// Store provides snapshot read and write access through a per-run directory
// layout: <root>/<runId>/snapshot.json.
type Store struct {
	root string
}

// DefaultRoot returns the default background-snapshot directory inside the
// user configuration tree: <UserConfigDir>/pi-worker/background.
func DefaultRoot() (string, error) {
	userDir, err := config.UserDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(userDir, "background"), nil
}

// NewStore creates a Store whose snapshots live under root. An empty root
// is rejected immediately without any filesystem mutation.
func NewStore(root string) (*Store, error) {
	if root == "" {
		return nil, fmt.Errorf("background: root must not be empty")
	}
	return &Store{root: root}, nil
}

// Load reads and validates the snapshot for the given run ID. It first
// verifies the run ID through [runlog.ParseRunID], then locates
// <root>/<runId>/snapshot.json using only strict checks:
//
//   - nil Store or empty root is rejected immediately without touching the
//     filesystem.
//   - The per-run directory is inspected with Lstat and must be a real
//     directory (not a symlink).
//   - The snapshot file is opened without following a final-component
//     symlink on darwin/linux (build-tag helper); other platforms also
//     Lstat-reject a final symlink before os.Open.
//   - Exactly one JSON document is decoded with DisallowUnknownFields;
//     trailing data is rejected, then Snapshot.Validate is called.
//   - The decoded Snapshot.RunID must equal the requested runId.
func (s *Store) Load(runID string) (Snapshot, error) {
	if s == nil || s.root == "" {
		return Snapshot{}, fmt.Errorf("load snapshot (%s): nil store or empty root", runID)
	}

	if _, err := runlog.ParseRunID(runID); err != nil {
		return Snapshot{}, fmt.Errorf("load snapshot (%s): %w", runID, err)
	}

	dir := filepath.Join(s.root, runID)
	path := snapshotPath(s.root, runID)

	// Inspect the per-run directory: must exist as a real directory.
	fi, err := os.Lstat(dir)
	if err != nil {
		return Snapshot{}, fmt.Errorf("load snapshot (%s): inspect directory %s: %w", runID, dir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return Snapshot{}, fmt.Errorf("load snapshot (%s): refusing to read through a symbolic link at %s", runID, dir)
	}
	if !fi.IsDir() {
		return Snapshot{}, fmt.Errorf("load snapshot (%s): %s exists but is not a directory", runID, dir)
	}

	// Inspect the snapshot file before opening: reject symlinks
	// and non-regular entries; no-follow open remains the race guard.
	si, lerr := os.Lstat(path)
	if lerr != nil {
		return Snapshot{}, fmt.Errorf("load snapshot (%s): inspect snapshot %s: %w", runID, path, lerr)
	}
	if si.Mode()&os.ModeSymlink != 0 {
		return Snapshot{}, fmt.Errorf("load snapshot (%s): refusing to read through a symbolic link at %s", runID, path)
	}
	if !si.Mode().IsRegular() {
		return Snapshot{}, fmt.Errorf("load snapshot (%s): %s exists but is not a regular file", runID, path)
	}

	f, err := openSnapshot(path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("load snapshot (%s): open snapshot %s: %w", runID, path, err)
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var snap Snapshot
	if err := dec.Decode(&snap); err != nil {
		return Snapshot{}, fmt.Errorf("load snapshot (%s): decode: %w", runID, err)
	}
	// Reject trailing data after the JSON document.
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return Snapshot{}, fmt.Errorf("load snapshot (%s): trailing data after document", runID)
		}
		return Snapshot{}, fmt.Errorf("load snapshot (%s): %w", runID, err)
	}

	// Require the decoded run ID matches the one we were asked to load.
	if snap.RunID != runID {
		return Snapshot{}, fmt.Errorf("load snapshot (%s): snapshot runId %q does not match requested %s", runID, snap.RunID, runID)
	}

	if err := snap.Validate(); err != nil {
		return Snapshot{}, fmt.Errorf("load snapshot (%s): validate: %w", runID, err)
	}

	return snap, nil
}

// Create writes a newly-built snapshot into <root>/<runId>/snapshot.json.
// It validates and encodes the snapshot before touching the filesystem,
// inspects (or creates) root, creates the per-run directory exclusively,
// and writes the file.  On failure after creating the run directory, one
// bounded cleanup removes only the exact entries we created.
func (s *Store) Create(snapshot Snapshot) error {
	if s == nil || s.root == "" {
		return fmt.Errorf("create snapshot: nil store or empty root")
	}
	if err := snapshot.Validate(); err != nil {
		return fmt.Errorf("create snapshot: validate: %w", err)
	}

	root := s.root
	runID := snapshot.RunID
	runDir := filepath.Join(root, runID)
	snapPath := snapshotPath(root, runID)

	// Encode before any filesystem mutation.
	encoded, err := encodeSnapshot(snapshot)
	if err != nil {
		return fmt.Errorf("create snapshot (%s): encode: %w", runID, err)
	}

	// Two-phase root preflight.
	fi, err := os.Lstat(root)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("create snapshot (%s): stat root %s: %w", runID, root, err)
		}
		// Root does not exist — create it, then re-check for raced replacements.
		if err := os.MkdirAll(root, 0o700); err != nil {
			return fmt.Errorf("create snapshot (%s): mkdir all root %s: %w", runID, root, err)
		}
		fi, err = os.Lstat(root)
		if err != nil {
			return fmt.Errorf("create snapshot (%s): stat root %s: %w", runID, root, err)
		}
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("create snapshot (%s): refusing to write through a symbolic link at %s", runID, root)
	}
	if !fi.IsDir() {
		return fmt.Errorf("create snapshot (%s): root %s exists but is not a directory", runID, root)
	}

	// Harden root permissions (off Windows).
	if runtime.GOOS != "windows" {
		if err := os.Chmod(root, 0700); err != nil {
			return fmt.Errorf("create snapshot (%s): chmod root %s: %w", runID, root, err)
		}
	}

	// Create the per-run directory exclusively.
	if err := os.Mkdir(runDir, 0700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("create snapshot (%s): run directory %s already exists", runID, runDir)
		}
		return fmt.Errorf("create snapshot (%s): mkdir run dir %s: %w", runID, runDir, err)
	}
	// Mark that we created the run directory for bounded cleanup.
	runDirCreated := true

	// Harden run directory permissions (off Windows).
	if runtime.GOOS != "windows" {
		if err := os.Chmod(runDir, 0700); err != nil {
			cErr := doCleanup(snapPath, runDir, root, runDirCreated, nil)
			return errors.Join(fmt.Errorf("create snapshot (%s): chmod run dir %s: %w", runID, runDir, err), cErr)
		}
	}

	// Write snapshot.json with O_CREATE|O_EXCL|O_WRONLY for exclusive creation.
	f, err := openSnapshotForCreate(snapPath)
	if err != nil {
		cErr := doCleanup(snapPath, runDir, root, runDirCreated, err)
		return fmt.Errorf("create snapshot (%s): open snapshot %s: %w", runID, snapPath, cErr)
	}

	// Write. Detect short write (matching Replace pattern).
	n, wErr := f.Write(encoded)
	if wErr == nil && n < len(encoded) {
		wErr = io.ErrShortWrite
	} else if wErr != nil && n < len(encoded) {
		wErr = errors.Join(wErr, io.ErrShortWrite)
	}

	// On write failure: close once, join close error, then cleanup. Do not call Sync.
	if wErr != nil {
		closeErr := f.Close()
		var cerr error
		if closeErr != nil {
			cerr = closeErr
		}
		cerr = errors.Join(cerr, wErr)
		cErr := doCleanup(snapPath, runDir, root, runDirCreated, cerr)
		return fmt.Errorf("create snapshot (%s): write snapshot %s: %w", runID, snapPath, cErr)
	}

	// Sync. On sync failure: close once, join close error, then cleanup.
	if sErr := f.Sync(); sErr != nil {
		closeErr := f.Close()
		var cerr error
		if closeErr != nil {
			cerr = closeErr
		}
		cerr = errors.Join(cerr, sErr)
		cErr := doCleanup(snapPath, runDir, root, runDirCreated, cerr)
		return fmt.Errorf("create snapshot (%s): sync snapshot %s: %w", runID, snapPath, cErr)
	}

	// Close. On close failure, run cleanup.
	if closeErr := f.Close(); closeErr != nil {
		cErr := doCleanup(snapPath, runDir, root, runDirCreated, closeErr)
		return fmt.Errorf("create snapshot (%s): close snapshot %s: %w", runID, snapPath, cErr)
	}

	// Sync the run directory.
	if err := syncParentDirectory(runDir); err != nil {
		cErr := doCleanup(snapPath, runDir, root, runDirCreated, err)
		return fmt.Errorf("create snapshot (%s): sync run dir %s: %w", runID, runDir, cErr)
	}

	// Sync the root directory.
	if err := syncParentDirectory(root); err != nil {
		cErr := doCleanup(snapPath, runDir, root, runDirCreated, err)
		return fmt.Errorf("create snapshot (%s): sync root %s: %w", runID, root, cErr)
	}

	return nil
}

// doCleanup performs a bounded cleanup of exactly the entries that [Create]
// added.  It removes the snapshot file, the run directory (if we created
// it), and syncs root afterward.
// Only fs.ErrNotExist is silently ignored during removal.  The returned
// error joins primaryErr (the reason for cleanup) with any cleanup-side
// errors, or nil when everything cleared cleanly.
func doCleanup(snapPath, runDir, root string, didCreateRunDir bool, primaryErr error) error {
	var errs []error

	// Remove the exact snapshot file (ignore only fs.ErrNotExist).
	if rErr := removeSnapshotForCleanup(snapPath); rErr != nil && !errors.Is(rErr, fs.ErrNotExist) {
		errs = append(errs, rErr)
	}

	// Remove the exact run directory only if we created it.
	if didCreateRunDir {
		if rErr := removeSnapshotForCleanup(runDir); rErr != nil && !errors.Is(rErr, fs.ErrNotExist) {
			errs = append(errs, rErr)
		}
		// Sync root after removing the run directory.
		if sErr := syncParentDirectory(root); sErr != nil {
			errs = append(errs, sErr)
		}
	}

	if primaryErr != nil {
		errs = append(errs, primaryErr)
	}

	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

// Replace atomically overwrites an existing snapshot at <root>/<runId>/snapshot.json
// with a new validated snapshot document.
//
// Both the per-run directory and the existing snapshot file must be real
// directory / regular file entries, not symlinks or special files.
// A temporary file is created inside the run directory, written, synced,
// closed, then renamed atomically over snapshot.json.  After rename,
// the run directory is synced.
//
// Failures before rename leave the previous snapshot intact.  A sync
// failure after rename returns an error but leaves one complete valid
// old-or-new JSON document.
func (s *Store) Replace(snapshot Snapshot) error {
	if s == nil || s.root == "" {
		return fmt.Errorf("replace snapshot: nil store or empty root")
	}
	if err := snapshot.Validate(); err != nil {
		return fmt.Errorf("replace snapshot: validate: %w", err)
	}

	// Encode before any filesystem operation.
	encoded, err := encodeSnapshot(snapshot)
	if err != nil {
		return fmt.Errorf("replace snapshot (%s): encode: %w", snapshot.RunID, err)
	}

	root := s.root
	runID := snapshot.RunID
	runDir := filepath.Join(root, runID)
	snapPath := snapshotPath(root, runID)

	// Inspect the per-run directory: must exist as a real directory.
	fi, err := os.Lstat(runDir)
	if err != nil {
		return fmt.Errorf("replace snapshot (%s): inspect run dir %s: %w", runID, runDir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("replace snapshot (%s): refusing to read through a symbolic link at %s", runID, runDir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("replace snapshot (%s): %s exists but is not a directory", runID, runDir)
	}

	// Inspect the existing snapshot file: must be a regular file, not a symlink.
	es, err := os.Lstat(snapPath)
	if err != nil {
		return fmt.Errorf("replace snapshot (%s): inspect snapshot %s: %w", runID, snapPath, err)
	}
	if es.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("replace snapshot (%s): refusing to replace through a symbolic link at %s", runID, snapPath)
	}
	if !es.Mode().IsRegular() {
		return fmt.Errorf("replace snapshot (%s): %s exists but is not a regular file", runID, snapPath)
	}

	// Create a temp file in the same run directory with owner-only permissions.
	f, err := createSnapshotTemp(runDir, ".snapshot.json.tmp-*")
	if err != nil {
		return fmt.Errorf("replace snapshot (%s): create temp file %s: %w", runID, runDir, err)
	}
	tmpName := f.Name()

	// Harden temp file permissions (off Windows).
	if runtime.GOOS != "windows" {
		if err := f.Chmod(0o600); err != nil {
			closeErr := f.Close()
			removeErr := removeSnapshotForCleanup(tmpName)
			if errors.Is(removeErr, fs.ErrNotExist) {
				removeErr = nil
			}
			return fmt.Errorf("replace snapshot (%s): chmod temp file %s: %w", runID, tmpName, errors.Join(closeErr, removeErr, err))
		}
	}

	n, writeErr := f.Write(encoded)
	if writeErr == nil && n != len(encoded) {
		writeErr = io.ErrShortWrite
	} else if writeErr != nil && n != len(encoded) {
		writeErr = errors.Join(writeErr, io.ErrShortWrite)
	}

	if writeErr != nil {
		closeErr := f.Close()
		removeErr := removeSnapshotForCleanup(tmpName)
		if errors.Is(removeErr, fs.ErrNotExist) {
			removeErr = nil
		}
		errs := []error{writeErr}
		if closeErr != nil {
			errs = append(errs, closeErr)
		}
		if removeErr != nil {
			errs = append(errs, removeErr)
		}
		return fmt.Errorf("replace snapshot (%s): write temp file %s: %w", runID, tmpName, errors.Join(errs...))
	}

	if sErr := f.Sync(); sErr != nil {
		closeErr := f.Close()
		removeErr := removeSnapshotForCleanup(tmpName)
		if errors.Is(removeErr, fs.ErrNotExist) {
			removeErr = nil
		}
		errs := []error{sErr}
		if closeErr != nil {
			errs = append(errs, closeErr)
		}
		if removeErr != nil {
			errs = append(errs, removeErr)
		}
		return fmt.Errorf("replace snapshot (%s): sync temp file %s: %w", runID, tmpName, errors.Join(errs...))
	}

	if closeErr := f.Close(); closeErr != nil {
		removeErr := removeSnapshotForCleanup(tmpName)
		if errors.Is(removeErr, fs.ErrNotExist) {
			removeErr = nil
		}
		errs := []error{closeErr}
		if removeErr != nil {
			errs = append(errs, removeErr)
		}
		return fmt.Errorf("replace snapshot (%s): close temp file %s: %w", runID, tmpName, errors.Join(errs...))
	}

	// Re-check snapPath as close to rename as possible (temp file already closed).
	rs, rerr := os.Lstat(snapPath)
	if rerr != nil || rs.Mode()&os.ModeSymlink != 0 || !rs.Mode().IsRegular() {
		tmpRmErr := removeSnapshotForCleanup(tmpName)
		if errors.Is(tmpRmErr, fs.ErrNotExist) {
			tmpRmErr = nil
		}
		baseErr := rerr
		if baseErr == nil {
			if rs.Mode()&os.ModeSymlink != 0 {
				baseErr = fmt.Errorf("refusing to replace through a symbolic link at %s", snapPath)
			} else {
				baseErr = fmt.Errorf("%s is not a regular file", snapPath)
			}
		}
		var reportErr error
		if tmpRmErr != nil {
			reportErr = errors.Join(baseErr, tmpRmErr)
		} else {
			reportErr = baseErr
		}
		return fmt.Errorf("replace snapshot (%s): %w", runID, reportErr)
	}

	// Atomic rename over snapshot.json.
	if err := renameSnapshot(tmpName, snapPath); err != nil {
		rmErr := removeSnapshotForCleanup(tmpName)
		var cleanupErr error
		if rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			cleanupErr = rmErr
		}
		return fmt.Errorf("replace snapshot (%s): rename %s → %s: %w", runID, tmpName, snapPath, errors.Join(cleanupErr, err))
	}

	// Sync the run directory after the atomic rename.
	if err := syncParentDirectory(runDir); err != nil {
		return fmt.Errorf("replace snapshot (%s): sync run dir %s: %w", runID, runDir, err)
	}

	return nil
}

// Remove deletes the snapshot for runID from the store. It rejects
// nil store/empty root and validates runID via [runlog.ParseRunID]
// before joining any paths.
//
// The per-run directory is Lstat-rejected if it is a symlink or not
// a directory (actual not-exist identities are preserved). The
// snapshot.json file is Lstat-rejected if it is a symlink or not a
// regular file (actual not-exist identities are preserved).
//
// Before any mutation the run directory is ReadDir'ed and must
// contain exactly one entry named "snapshot.json" — this method never
// deletes future control files or unrelated state. Only
// snapshot.json is removed, the run directory is synced, the now-
// empty run directory is removed, and finally root is synced.
//
// Returns operation/path-specific errors. After any successful
// removal step a later sync/remove failure reports the real partial
// cleanup state; rollback is not attempted, the call is not made
// idempotent, no recursive deletion occurs.
func (s *Store) Remove(runID string) error {
	if s == nil || s.root == "" {
		return fmt.Errorf("remove snapshot (%s): nil store or empty root", runID)
	}

	if _, err := runlog.ParseRunID(runID); err != nil {
		return fmt.Errorf("remove snapshot (%s): %w", runID, err)
	}

	root := s.root
	runDir := filepath.Join(root, runID)
	snapPath := snapshotPath(root, runID)

	// Inspect the per-run directory: must exist as a real directory.
	fi, err := os.Lstat(runDir)
	if err != nil {
		return fmt.Errorf("remove snapshot (%s): inspect run dir %s: %w", runID, runDir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("remove snapshot (%s): refusing to remove through a symbolic link at %s", runID, runDir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("remove snapshot (%s): %s exists but is not a directory", runID, runDir)
	}

	// Inspect the snapshot file: must be a regular file, not a symlink.
	esi, err := os.Lstat(snapPath)
	if err != nil {
		return fmt.Errorf("remove snapshot (%s): inspect snapshot %s: %w", runID, snapPath, err)
	}
	if esi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("remove snapshot (%s): refusing to remove through a symbolic link at %s", runID, snapPath)
	}
	if !esi.Mode().IsRegular() {
		return fmt.Errorf("remove snapshot (%s): %s exists but is not a regular file", runID, snapPath)
	}

	// Require exactly one entry named "snapshot.json" so this method
	// never deletes future control files or unrelated state.
	entries, err := os.ReadDir(runDir)
	if err != nil {
		return fmt.Errorf("remove snapshot (%s): read run dir %s: %w", runID, runDir, err)
	}
	if len(entries) != 1 || entries[0].Name() != "snapshot.json" {
		return fmt.Errorf("remove snapshot (%s): %s does not contain exactly one snapshot.json entry", runID, runDir)
	}

	// Remove only the snapshot file.
	if err := os.Remove(snapPath); err != nil {
		return fmt.Errorf("remove snapshot (%s): remove %s: %w", runID, snapPath, err)
	}

	// Sync the run directory after removing its only entry.
	if err := syncParentDirectory(runDir); err != nil {
		return fmt.Errorf("remove snapshot (%s): sync run dir %s: %w", runID, runDir, err)
	}

	// Remove only the now-empty run directory.
	if err := os.Remove(runDir); err != nil {
		return fmt.Errorf("remove snapshot (%s): remove run dir %s: %w", runID, runDir, err)
	}

	// Sync root after removing the run directory.
	if err := syncParentDirectory(root); err != nil {
		return fmt.Errorf("remove snapshot (%s): sync root %s: %w", runID, root, err)
	}

	return nil
}

// encodeSnapshot marshals s as compact JSON followed by exactly one newline.
func encodeSnapshot(s Snapshot) ([]byte, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	return data, nil
}

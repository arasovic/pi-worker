package admission

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// statePath returns root/state.json for the given root directory.
func statePath(root string) string {
	return filepath.Join(root, "state.json")
}

// maxStateBytes is the ceiling on the state file's size, in bytes.
// A real state document is a handful of tickets of JSON — kilobytes
// — so 32 MiB is four orders of magnitude of headroom and still
// cannot exhaust memory. loadState refuses a state above the ceiling
// before it is read. The headroom and reasoning match maxRecordBytes
// in internal/runlog: the two documents are comparably small and the
// same ceiling keeps either from exhausting memory.
const maxStateBytes int64 = 32 << 20

// beforeStateOpen is called once by loadState between its check of
// the state path and the open of that path. The product never assigns
// to it and always runs the no-op below; the variable exists only so
// a test can replace it with a function that changes the path's
// target inside that window and pin the re-check after the open.
var beforeStateOpen = func() {}

// loadState reads and validates the admission state document at root/state.json.
// A missing file is treated as a valid empty state. A symbolic link at the
// final path is rejected outright — whether its target exists or is dangling —
// as is anything that is not a regular file and anything above maxStateBytes,
// before the path is opened or read. After the open, the file that was
// opened is checked itself: it must still be a regular file, it must be the
// very file the checks described, and it must still be within the size
// ceiling, so a name replaced between the check and the open is refused
// rather than read. The open itself is non-blocking, so a name that became
// a named pipe between the check and the open opens immediately instead of
// blocking forever on a writer that never comes, and the re-check then
// refuses the pipe for what it is. A refused state is an error, never an
// empty state: only a missing file is an empty state. The load rejects
// unknown JSON fields, trailing data, malformed or corrupt documents, and
// invalid state — all without modifying the file on disk.
func loadState(root string) (state, error) {
	path := statePath(root)

	// Reject any symbolic link before following it.
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return emptyState(), nil
		}
		return state{}, fmt.Errorf("load admission state %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return state{}, fmt.Errorf("load admission state %s: refusing to read through a symbolic link", path)
	}
	if !fi.Mode().IsRegular() {
		return state{}, fmt.Errorf("load admission state %s: state is not a regular file", path)
	}
	if fi.Size() < 0 || fi.Size() > maxStateBytes {
		return state{}, fmt.Errorf("load admission state %s: state is too large (%d bytes exceeds %d bytes)", path, fi.Size(), maxStateBytes)
	}

	// The test-only seam: between the checks above and the open
	// below, a no-op in the product.
	beforeStateOpen()

	f, err := openState(path)
	if err != nil {
		return state{}, fmt.Errorf("load admission state %s: %w", path, err)
	}
	defer f.Close()

	opened, err := f.Stat()
	if err != nil {
		return state{}, fmt.Errorf("load admission state %s: %w", path, err)
	}
	// The re-check of the open file: a name replaced between the
	// check and the open hands the open a different file, and a file
	// that grew past the ceiling after the check must not be read
	// whole. The first condition also refuses a symlink that
	// appeared in the gap.
	if !opened.Mode().IsRegular() || !os.SameFile(fi, opened) {
		return state{}, fmt.Errorf("load admission state %s: state changed before reading", path)
	}
	if opened.Size() < 0 || opened.Size() > maxStateBytes {
		return state{}, fmt.Errorf("load admission state %s: state is too large (%d bytes exceeds %d bytes)", path, opened.Size(), maxStateBytes)
	}

	dec := json.NewDecoder(io.LimitReader(f, maxStateBytes))
	dec.DisallowUnknownFields()
	var s state
	if err := dec.Decode(&s); err != nil {
		return state{}, fmt.Errorf("load admission state %s: %v", path, err)
	}
	// Reject trailing data after the JSON document.
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return state{}, fmt.Errorf("load admission state %s: trailing data after document", path)
		}
		return state{}, fmt.Errorf("load admission state %s: %v", path, err)
	}

	if err := validateState(s); err != nil {
		return state{}, fmt.Errorf("load admission state %s: %w", path, err)
	}
	return s, nil
}

// saveState writes state to root/state.json atomically. The document is validated
// before the existing file is touched. The final state path must not be a
// symbolic link. The root directory is created and tightened to owner-only
// permissions where supported. The new content is written to a temporary file
// in the same directory with owner-only permissions, synced to disk, and
// renamed over the destination; finally the parent directory is synced where
// the platform supports it. A save failure leaves the previous state intact
// and removes any temporary file.
func saveState(root string, s state) error {
	if err := validateState(s); err != nil {
		return err
	}
	path := statePath(root)

	// The destination itself must never be a symbolic link.
	info, err := os.Lstat(path)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("save admission state %s: refusing to replace or write through a symbolic link", path)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("save admission state %s: inspect path: %w", path, err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("save admission state %s: create directory: %w", path, err)
	}
	// Tighten the root directory to owner-only where supported.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("save admission state %s: set directory permissions: %w", path, err)
	}

	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("save admission state %s: encode: %w", path, err)
	}
	data = append(data, '\n')

	base := filepath.Base(path)
	tmp, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return fmt.Errorf("save admission state %s: create temporary file: %w", path, err)
	}
	tmpName := tmp.Name()
	remove := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		remove()
		return fmt.Errorf("save admission state %s: set permissions: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		remove()
		return fmt.Errorf("save admission state %s: write: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		remove()
		return fmt.Errorf("save admission state %s: sync: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("save admission state %s: close: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("save admission state %s: replace: %w", path, err)
	}
	if err := syncParentDirectory(dir); err != nil {
		return fmt.Errorf("save admission state %s: sync parent directory: %w", path, err)
	}
	return nil
}

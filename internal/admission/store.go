package admission

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// StatePath returns root/state.json for the given root directory.
func StatePath(root string) string {
	return filepath.Join(root, "state.json")
}

// Load reads and validates the admission state document at root/state.json.
// A missing file is treated as a valid empty state. The load rejects
// unknown JSON fields, trailing data, malformed or corrupt documents,
// and invalid state — all without modifying the file on disk.
func Load(root string) (State, error) {
	path := StatePath(root)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return EmptyState(), nil
		}
		return State{}, fmt.Errorf("load admission state %s: %w", path, err)
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var wire wireState
	if err := dec.Decode(&wire); err != nil {
		return State{}, fmt.Errorf("load admission state %s: %v", path, err)
	}
	// Reject trailing data after the JSON document.
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return State{}, fmt.Errorf("load admission state %s: trailing data after document", path)
		}
		return State{}, fmt.Errorf("load admission state %s: %v", path, err)
	}

	state := coerceWireState(wire)
	if err := Validate(state); err != nil {
		return State{}, fmt.Errorf("load admission state %s: %w", path, err)
	}
	return state, nil
}

// Save writes state to root/state.json atomically. The document is validated
// before the existing file is touched. The final state path must not be a
// symbolic link. The root directory is created and tightened to owner-only
// permissions where supported. The new content is written to a temporary file
// in the same directory with owner-only permissions, synced to disk, and
// renamed over the destination; finally the parent directory is synced where
// the platform supports it. A save failure leaves the previous state intact
// and removes any temporary file.
func Save(root string, state State) error {
	if err := Validate(state); err != nil {
		return err
	}
	path := StatePath(root)

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
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("save admission state %s: set directory permissions: %w", path, err)
		}
	}

	data, err := json.Marshal(state)
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
	if runtime.GOOS != "windows" {
		if err := tmp.Chmod(0o600); err != nil {
			remove()
			return fmt.Errorf("save admission state %s: set permissions: %w", path, err)
		}
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
	if runtime.GOOS != "windows" {
		if err := syncParentDirectory(dir); err != nil {
			return fmt.Errorf("save admission state %s: sync parent directory: %w", path, err)
		}
	}
	return nil
}

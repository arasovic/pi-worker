package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

var chmodConfigDirectory = os.Chmod

// Load reads and validates the configuration document at path. It rejects
// unknown fields, trailing JSON after the document, unsupported schema
// versions, and invalid default model selectors.
func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("load config %s: %v", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return Config{}, fmt.Errorf("load config %s: trailing data after document", path)
		}
		return Config{}, fmt.Errorf("load config %s: %v", path, err)
	}
	if err := Validate(cfg); err != nil {
		return Config{}, fmt.Errorf("load config %s: %w", path, err)
	}
	return cfg, nil
}

// Save writes cfg to path atomically. The document is validated before the
// existing file is touched. A path whose final component is a symbolic link —
// its target present or dangling — is refused before anything is touched: the
// save never replaces the link and never writes through it, so the link and
// its target stay exactly as they were. Missing parent directories are
// created, the new content is written to a temporary file in the same
// directory with owner-only permissions, synced to disk, and renamed over the
// destination; finally the parent directory is synced where the platform
// supports it.
func Save(path string, cfg Config) error {
	if err := Validate(cfg); err != nil {
		return err
	}
	// The destination itself must never be a symbolic link. Replacing the link
	// with a regular file would silently tear down an arrangement the user
	// built — a dotfiles link, say — leaving its target stale and unreferenced,
	// and writing through it would overwrite whatever the link points at, so
	// both are refused up front, before the directory, the link, or the target
	// is modified in any way. Only the final component is inspected, so a
	// symlinked parent directory keeps working: the temporary file and the
	// rename land in the real directory the parent link names. os.Lstat
	// reports the link entry itself, so a dangling link — one whose target
	// does not exist — is refused too; only a path whose final component does
	// not exist at all is a plain new file and proceeds normally.
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("save config %s: refusing to replace or write through a symbolic link", path)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("save config %s: create directory: %w", path, err)
	}
	if runtime.GOOS != "windows" {
		userPath, err := UserPath()
		if err == nil && filepath.Clean(path) == filepath.Clean(userPath) {
			if err := chmodConfigDirectory(dir, 0o700); err != nil {
				return fmt.Errorf("save config %s: set directory permissions: %w", path, err)
			}
		}
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("save config %s: encode: %w", path, err)
	}
	data = append(data, '\n')

	base := filepath.Base(path)
	tmp, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return fmt.Errorf("save config %s: create temporary file: %w", path, err)
	}
	tmpName := tmp.Name()
	remove := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if runtime.GOOS != "windows" {
		// Windows does not support Unix permission bits; everywhere else
		// the temporary file must be owner-only.
		if err := tmp.Chmod(0o600); err != nil {
			remove()
			return fmt.Errorf("save config %s: set permissions: %w", path, err)
		}
	}
	if _, err := tmp.Write(data); err != nil {
		remove()
		return fmt.Errorf("save config %s: write: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		remove()
		return fmt.Errorf("save config %s: sync: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("save config %s: close: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("save config %s: replace: %w", path, err)
	}
	if runtime.GOOS != "windows" {
		if err := syncParentDirectory(dir); err != nil {
			return fmt.Errorf("save config %s: sync parent directory: %w", path, err)
		}
	}
	return nil
}

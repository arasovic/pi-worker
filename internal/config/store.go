package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

var chmodConfigDirectory = os.Chmod

// errDanglingConfigLink marks the one not-exist answer a configuration read
// must not treat as a missing file: the final config.json entry itself is a
// symbolic link whose target does not exist. A load through it is neither an
// empty configuration nor an ordinary unreadable document; the caller names
// it on every read path and leaves the link untouched. The sentinel is
// compared by identity, never through error strings.
var errDanglingConfigLink = errors.New("config path is a symbolic link whose target does not exist")

// Load reads and validates the configuration document at path. It rejects
// unknown fields, trailing JSON after the document, unsupported schema
// versions, and invalid default model selectors.
//
// A missing configuration document is a valid empty configuration, so the
// absence of the file is reported as the original not-exist error, never as
// a validation failure. A config.json that is itself a symbolic link whose
// target is missing is different: the path holds a broken link, and reading
// through it must fail clearly on every read path — config show, run, and
// doctor all share this one Load — while leaving the link untouched. That
// answer is wrapped so every caller can recognise it, and is returned only
// for the final component: a dangling link in a parent directory is a plain
// missing config.json and still reports the ordinary not-exist error.
func Load(path string) (Config, error) {
	f, err := openConfig(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	return loadFile(path, f)
}

// openConfig opens the configuration document at path for reading. A final
// component that is a dangling symbolic link is refused with the wrapped
// dangling-link error — the file can never be read, so the open answers
// that clearly instead of letting the caller read the not-exist error as a
// missing configuration. The distinction is made only after an open has
// returned not-exist, so an absent file reports the operating system's own
// error unchanged and a symlink with a present target keeps resolving.
// Only the final path entry is inspected, so a symlinked parent directory
// continues to resolve like any other open.
func openConfig(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	// The open followed the path and the last component was not there. Only
	// the final entry can say whether that is an absent file or a symlink
	// whose target is absent: Lstat inspects the entry itself. A symlinked
	// entry is the dangling-link case and is wrapped clearly. An entry that
	// is not a symlink — or that Lstat also cannot find — is a truly absent
	// file, and the original not-exist error keeps its identity, so callers
	// that read absence as an empty configuration still do. An Lstat failure
	// of its own is reported as itself, never as either answer.
	info, lerr := os.Lstat(path)
	if lerr != nil && !errors.Is(lerr, fs.ErrNotExist) {
		return nil, lerr
	}
	if lerr == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("read config %s: %w", path, errDanglingConfigLink)
	}
	return nil, err
}

// wireConfig is the JSON wire format for configuration documents.
// MaxModelWorkers is json.RawMessage so that Load can distinguish omission
// (schema 1) from an explicit JSON null, a number, or any other value.
type wireConfig struct {
	SchemaVersion   int             `json:"schemaVersion"`
	DefaultModel    string          `json:"defaultModel"`
	MaxModelWorkers json.RawMessage `json:"maxModelWorkers"`
}

// loadFile decodes and validates one configuration document from r. The
// caller has already opened the file and reports its own open errors, so
// decode and validation failures are the only errors that surface here.
func loadFile(path string, r io.Reader) (Config, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var wire wireConfig
	if err := dec.Decode(&wire); err != nil {
		return Config{}, fmt.Errorf("load config %s: %v", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return Config{}, fmt.Errorf("load config %s: trailing data after document", path)
		}
		return Config{}, fmt.Errorf("load config %s: %v", path, err)
	}

	var cfg Config
	switch wire.SchemaVersion {
	case 1:
		// Schema 1 permits only schemaVersion and defaultModel.
		if wire.MaxModelWorkers != nil {
			return Config{}, fmt.Errorf("load config %s: maxModelWorkers is not valid in schema version 1", path)
		}
		cfg = Config{
			SchemaVersion:   2,
			DefaultModel:    wire.DefaultModel,
			MaxModelWorkers: 3,
		}
	case 2:
		if wire.MaxModelWorkers == nil {
			return Config{}, fmt.Errorf("load config %s: maxModelWorkers is required in schema version 2", path)
		}
		if isJSONNull(wire.MaxModelWorkers) {
			return Config{}, fmt.Errorf("load config %s: maxModelWorkers is required in schema version 2, got null", path)
		}
		mw, err := parsePositiveInt(wire.MaxModelWorkers)
		if err != nil {
			return Config{}, fmt.Errorf("load config %s: maxModelWorkers must be a positive integer: %v", path, err)
		}
		cfg = Config{
			SchemaVersion:   2,
			DefaultModel:    wire.DefaultModel,
			MaxModelWorkers: mw,
		}
	default:
		return Config{}, fmt.Errorf("load config %s: unsupported schemaVersion %d", path, wire.SchemaVersion)
	}

	if err := Validate(cfg); err != nil {
		return Config{}, fmt.Errorf("load config %s: %w", path, err)
	}
	return cfg, nil
}

// isJSONNull reports whether raw is a JSON null literal.
func isJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// parsePositiveInt decodes raw as a JSON integer and returns it if it is
// strictly positive. An error is returned for non-integer values, zero,
// or negative numbers.
func parsePositiveInt(raw json.RawMessage) (int, error) {
	var v int
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, fmt.Errorf("%s: not an integer", string(raw))
	}
	if v <= 0 {
		return 0, fmt.Errorf("%d: not positive", v)
	}
	return v, nil
}

// Save writes cfg to path atomically. The document is validated before the
// existing file is touched. A path whose final component is a symbolic link —
// its target present or dangling — is refused before anything is touched: the
// save never replaces or writes through the link it inspected, so the link
// and its target stay exactly as they were. The refusal is the write half of
// the link contract; the read half lives in Load, which follows a link with
// a present target and fails clearly on a dangling one instead of reading it
// as a missing file. Missing parent directories are created, the new content
// is written to a temporary file in the same directory with owner-only
// permissions, synced to disk, and renamed over the destination; finally the
// parent directory is synced where the platform supports it.
//
// The refusal is a check at one instant, not a lock on the path: another
// process can replace the destination after the guard has passed it, and the
// atomic rename at the end then replaces whatever holds the name — a
// swapped-in link included. No protection against that concurrent replacement
// is claimed. The guarantee is exact and narrower: a final component observed
// as a symbolic link is refused before anything is touched, and the write is
// a rename over the destination name, never a follow, so a save can replace a
// link but never write through one into its target.
func Save(path string, cfg Config) error {
	if err := Validate(cfg); err != nil {
		return err
	}
	// The destination itself must never be a symbolic link. Replacing the link
	// with a regular file would silently tear down an arrangement the user
	// built — a dotfiles link, say — leaving its target stale and
	// unreferenced, and writing through it would overwrite whatever the link
	// points at, so both are refused up front, before the directory, the link,
	// or the target is modified in any way. os.Lstat reports the entry itself,
	// so a dangling link — one whose target does not exist — is refused too.
	// Only the final component is inspected, so a symlinked parent directory
	// keeps working: the temporary file and the rename land in the real
	// directory the parent link names. A missing final component is a plain
	// new destination and proceeds; any other Lstat failure — a path the
	// operating system cannot inspect — is returned as a wrapped inspect-path
	// error before MkdirAll or any other mutation.
	info, err := os.Lstat(path)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("save config %s: refusing to replace or write through a symbolic link", path)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("save config %s: inspect path: %w", path, err)
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

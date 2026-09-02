// Package config provides the foundational personal configuration document
// for pi-worker: a versioned JSON file storing only the default model
// selector. The schema is deliberately minimal and provider-neutral.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// schemaVersion is the only supported document version.
const schemaVersion = 1

// Config is the personal configuration document for pi-worker.
type Config struct {
	SchemaVersion int    `json:"schemaVersion"`
	DefaultModel  string `json:"defaultModel"`
}

// Empty returns a valid configuration with no default model.
func Empty() Config {
	return Config{SchemaVersion: schemaVersion}
}

// UserDir returns the default configuration directory for pi-worker.
func UserDir() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "pi-worker"), nil
}

// UserPath returns the default location of the personal configuration file
// inside the current user's configuration directory.
func UserPath() (string, error) {
	dir, err := UserDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Validate reports whether cfg is a well-formed configuration document.
// The schema version must be 1. DefaultModel may be empty; a non-empty
// value must be an exact provider/model selector: it must split at its
// first slash into a non-empty provider and a non-empty id. Nothing about
// the id's contents is inspected: the catalog a default is chosen from is
// the authority on whether a name is usable, and this rule only decides
// whether a name has a shape a name can have. The one asymmetry is the
// provider half: a selector names the provider as everything before the
// first slash, so a catalog entry whose provider itself contains a slash
// can never be named by any selector.
func Validate(cfg Config) error {
	if cfg.SchemaVersion != schemaVersion {
		return fmt.Errorf("unsupported schemaVersion %d: want %d", cfg.SchemaVersion, schemaVersion)
	}
	if cfg.DefaultModel == "" {
		return nil
	}
	model := cfg.DefaultModel
	provider, id, found := strings.Cut(model, "/")
	if !found {
		return fmt.Errorf("invalid defaultModel %q: must be provider/id", model)
	}
	if provider == "" || id == "" {
		return fmt.Errorf("invalid defaultModel %q: provider and id must both be non-empty", model)
	}
	return nil
}

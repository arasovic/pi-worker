// Package config provides the foundational personal configuration document
// for pi-worker: a versioned JSON file storing only the default model
// selector. The schema is deliberately minimal and provider-neutral.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
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
// value must be an exact provider/model selector: a single slash with
// non-empty provider and id parts and no additional slash, colon, or
// whitespace.
func Validate(cfg Config) error {
	if cfg.SchemaVersion != schemaVersion {
		return fmt.Errorf("unsupported schemaVersion %d: want %d", cfg.SchemaVersion, schemaVersion)
	}
	if cfg.DefaultModel == "" {
		return nil
	}
	model := cfg.DefaultModel
	if strings.ContainsAny(model, ":") || strings.IndexFunc(model, unicode.IsSpace) >= 0 {
		return fmt.Errorf("invalid defaultModel %q: must be provider/id", model)
	}
	provider, id, found := strings.Cut(model, "/")
	if !found {
		return fmt.Errorf("invalid defaultModel %q: must be provider/id", model)
	}
	if provider == "" || id == "" {
		return fmt.Errorf("invalid defaultModel %q: provider and id must both be non-empty", model)
	}
	return nil
}

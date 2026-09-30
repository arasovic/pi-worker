//go:build !darwin && !linux

package admission

import (
	"os"
)

// openState falls back to a plain open on platforms where the admission
// Gate itself is unsupported. It exists only so the package compiles
// there; darwin and linux use a no-follow, non-blocking open instead.
// The return type matches the unix open: *os.File so the caller can
// stat the opened file.
func openState(path string) (*os.File, error) {
	return os.Open(path)
}

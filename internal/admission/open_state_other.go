//go:build !darwin && !linux

package admission

import (
	"io"
	"os"
)

// openState falls back to a plain open on platforms where the admission
// Gate itself is unsupported. It exists only so the package compiles
// there; darwin and linux use a no-follow open instead.
func openState(path string) (io.ReadCloser, error) {
	return os.Open(path)
}

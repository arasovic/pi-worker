//go:build darwin || linux

package admission

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// openState opens path read-only without following a symbolic link at the
// final path component, so a symlink swapped in after a preceding Lstat can
// never be followed. The returned file is close-on-exec. An attempt to open
// through a symlink fails with an error wrapping ELOOP. Other open failures
// are reported unchanged.
func openState(path string) (io.ReadCloser, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open state: %w", err)
	}
	return f, nil
}

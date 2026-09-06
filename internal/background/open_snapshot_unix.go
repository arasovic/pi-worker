//go:build darwin || linux

package background

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// openSnapshot opens path read-only without following a symbolic link at the
// final path component, so a symlink swapped in after a preceding Lstat can
// never be followed. The returned file descriptor carries close-on-exec. An
// attempt to open through a symlink fails with an error wrapping ELOOP.
// Missing files return fs.ErrNotExist unchanged.  Other open failures pass
// through as-is.
func openSnapshot(path string) (io.ReadCloser, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open snapshot %s: %w", path, err)
	}
	return f, nil
}

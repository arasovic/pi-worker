//go:build darwin || linux

package background

import (
	"fmt"
	"os"
	"syscall"
)

// openSnapshot opens path read-only without following a symbolic link at the
// final path component, so a symlink swapped in after a preceding Lstat can
// never be followed, and without blocking on a named pipe swapped in after
// the preceding checks: O_NONBLOCK keeps the open of a writerless pipe from
// blocking forever, and the caller re-checks the opened file and refuses it
// for what it is. The returned file is close-on-exec. An attempt to open
// through a symlink fails with an error wrapping ELOOP. Missing files return
// fs.ErrNotExist unchanged. Other open failures pass through as-is. The
// caller stats the returned file to pin the re-check after the open, so the
// return type is *os.File rather than an interface.
func openSnapshot(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open snapshot %s: %w", path, err)
	}
	return f, nil
}

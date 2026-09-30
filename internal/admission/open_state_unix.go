//go:build darwin || linux

package admission

import (
	"fmt"
	"os"
	"syscall"
)

// openState opens path read-only without following a symbolic link at the
// final path component, so a symlink swapped in after a preceding Lstat can
// never be followed, and without blocking on a named pipe swapped in after
// the preceding checks: O_NONBLOCK keeps the open of a writerless pipe from
// blocking forever, and the caller re-checks the opened file and refuses it
// for what it is. The returned file is close-on-exec. An attempt to open
// through a symlink fails with an error wrapping ELOOP. Other open failures
// are reported unchanged. The caller stats the returned file to pin the
// re-check after the open, so the return type is *os.File rather than an
// interface.
func openState(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open state: %w", err)
	}
	return f, nil
}

//go:build darwin || linux || freebsd || openbsd || netbsd

package runlog

import "syscall"

// openNonBlock is the flag readRecordFile adds to the read-only
// flags when it opens a record path: the open must not block on a
// name that became a named pipe with no writer between the check
// and the open, because a blocked open cannot be interrupted. On a
// regular file the flag does nothing.
//
// The build tag above selects this file on the platforms whose
// syscall package carries both the flag and the named-pipe call;
// the other file is the zero fallback for the rest.
const openNonBlock = syscall.O_NONBLOCK

// mkfifo creates a named pipe at path, the one non-portable piece
// of the test that plants a pipe under a record name; on the
// platforms without named pipes it fails, and the test skips itself.
func mkfifo(path string) error {
	return syscall.Mkfifo(path, 0o600)
}

//go:build !darwin && !linux

package runlog

import "errors"

// openNonBlock is zero on the platforms whose syscall package has
// no O_NONBLOCK — plan9 among them — so the record open stays
// blocking there: the one ceiling readRecordFile's comment names.
const openNonBlock = 0

// mkfifo is the non-portable half of the named-pipe test: on these
// platforms a named pipe cannot be created, the call fails, and the
// test skips itself.
func mkfifo(path string) error {
	return errors.New("named pipes are not supported on this platform")
}

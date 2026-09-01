//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd

package runlog

import "errors"

// openNonBlock is zero on the platforms this fallback file builds
// for: those whose syscall package carries neither the flag nor the
// named-pipe call, and windows — which has the flag but no named
// pipes reachable through a filesystem path — so the record open
// stays blocking there: the one ceiling readRecordFile's comment
// names.
const openNonBlock = 0

// mkfifo is the non-portable half of the named-pipe test: on these
// platforms a named pipe cannot be created, the call fails, and the
// test skips itself.
func mkfifo(path string) error {
	return errors.New("named pipes are not supported on this platform")
}

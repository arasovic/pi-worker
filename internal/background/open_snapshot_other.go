//go:build !darwin && !linux

package background

import (
	"fmt"
	"io"
	"os"
)

// openSnapshot refuses a symbolic link at the final path component and
// opens the snapshot file read-only.  A Lstat inspects the entry itself so
// a symlink in a parent directory resolves normally while the final entry
// — the snapshot.json file — is checked for a dangling or present symlink.
func openSnapshot(path string) (io.ReadCloser, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		// The final component is a symbolic link; reject it rather than
		// following it into an arbitrary target.
		return nil, fmt.Errorf("snapshot %s: refusing to read through a symbolic link", path)
	}
	return os.Open(path)
}

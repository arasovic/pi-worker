//go:build darwin || linux

package skillinstall

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func openNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open without following symlinks: %w", err)
	}
	return os.NewFile(uintptr(fd), path), nil
}

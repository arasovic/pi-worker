//go:build darwin || linux

package admission

import (
	"errors"
	"syscall"
)

func isUnsupportedDirectorySync(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP)
}

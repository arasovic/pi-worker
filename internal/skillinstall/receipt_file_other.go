//go:build !darwin && !linux

package skillinstall

import "os"

func openNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}

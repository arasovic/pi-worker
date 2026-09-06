//go:build !darwin && !linux

package background

func isUnsupportedDirectorySync(error) bool { return false }

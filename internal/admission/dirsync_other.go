//go:build !darwin && !linux

package admission

func isUnsupportedDirectorySync(error) bool { return false }

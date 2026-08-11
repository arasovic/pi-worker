//go:build !darwin && !linux

package config

func isUnsupportedDirectorySync(error) bool { return false }

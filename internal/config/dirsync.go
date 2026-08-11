package config

import (
	"errors"
	"os"
)

type directorySyncHandle interface {
	Sync() error
	Close() error
}

var openDirectoryForSync = func(path string) (directorySyncHandle, error) {
	return os.Open(path)
}

func syncParentDirectory(path string) error {
	dir, err := openDirectoryForSync(path)
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if isUnsupportedDirectorySync(syncErr) {
		return closeErr
	}
	return errors.Join(syncErr, closeErr)
}

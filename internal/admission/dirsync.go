package admission

import (
	"errors"
	"os"
)

func syncParentDirectory(path string) error {
	dir, err := os.Open(path)
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

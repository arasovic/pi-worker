package skillinstall

import (
	"errors"
	"fmt"
	"io"
	"os"
)

const maxReceiptBytes int64 = 1 << 20

func readReceiptBytes(path string) (data []byte, err error) {
	return readStableRegularFile(path, maxReceiptBytes)
}

func readBoundedRegularFile(path string, maxBytes int64) (data []byte, err error) {
	return readStableRegularFile(path, maxBytes)
}

func readStableRegularFile(path string, maxBytes int64) (data []byte, err error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("path is not a regular file")
	}
	if before.Size() < 0 || before.Size() > maxBytes {
		return nil, fmt.Errorf("file is too large")
	}

	f, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
			if err != nil {
				data = nil
			}
		}
	}()

	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, fmt.Errorf("file changed before reading")
	}
	if opened.Size() < 0 || opened.Size() > maxBytes || opened.Size() != before.Size() {
		return nil, fmt.Errorf("file size changed before reading")
	}

	data, err = io.ReadAll(io.LimitReader(f, maxBytes))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != opened.Size() {
		return nil, fmt.Errorf("file size changed while reading")
	}

	after, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, fmt.Errorf("file changed after reading")
	}
	if after.Size() != before.Size() || after.Size() != opened.Size() {
		return nil, fmt.Errorf("file size changed after reading")
	}
	return data, nil
}

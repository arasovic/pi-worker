package releaseartifact

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type archiveEntry struct {
	name   string
	source string
	mode   int64
}

var openArchiveOutput = func(path string) (io.WriteCloser, error) {
	return os.Create(path)
}

var openArchiveSource = func(path string) (io.ReadCloser, error) {
	return os.Open(path)
}

// createArchive creates a deterministic tar.gz archive from one binary and two
// metadata files. Caller controls the archive contents' header timestamp.
func createArchive(binaryPath, licensePath, noticePath, outputPath string, buildDate time.Time) (err error) {
	entries := []archiveEntry{
		{name: "LICENSE", source: licensePath, mode: 0o644},
		{name: "THIRD_PARTY_NOTICES", source: noticePath, mode: 0o644},
		{name: "pi-worker", source: binaryPath, mode: 0o755},
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].name < entries[j].name
	})

	output, err := openArchiveOutput(outputPath)
	if err != nil {
		return fmt.Errorf("create archive: %w", err)
	}

	gz := gzip.NewWriter(output)
	gz.Header.ModTime = time.Time{}
	gz.Header.Name = ""
	gz.Header.Comment = ""
	gz.Header.Extra = nil
	gz.Header.OS = 255
	tw := tar.NewWriter(gz)
	defer func() {
		if closeErr := tw.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close tar writer: %w", closeErr))
		}
		if closeErr := gz.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close gzip writer: %w", closeErr))
		}
		if closeErr := output.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close archive output: %w", closeErr))
		}
	}()

	for _, entry := range entries {
		info, err := os.Stat(entry.source)
		if err != nil {
			return fmt.Errorf("read archive source %s: %w", entry.name, err)
		}

		sourceFile, err := openArchiveSource(entry.source)
		if err != nil {
			return fmt.Errorf("open archive source %s: %w", entry.name, err)
		}

		hdr := &tar.Header{
			Name:    filepath.Clean(entry.name),
			Size:    info.Size(),
			Mode:    entry.mode,
			ModTime: buildDate,
			Uid:     0,
			Gid:     0,
		}
		writeErr := error(nil)
		if err := tw.WriteHeader(hdr); err != nil {
			writeErr = fmt.Errorf("write archive header %s: %w", entry.name, err)
		} else if _, err := io.Copy(tw, sourceFile); err != nil {
			writeErr = fmt.Errorf("write archive content %s: %w", entry.name, err)
		}
		sourceCloseErr := sourceFile.Close()
		if writeErr != nil {
			if sourceCloseErr != nil {
				return errors.Join(writeErr, fmt.Errorf("close archive source %s: %w", entry.name, sourceCloseErr))
			}
			return writeErr
		}
		if sourceCloseErr != nil {
			return fmt.Errorf("close archive source %s: %w", entry.name, sourceCloseErr)
		}
	}

	return nil
}

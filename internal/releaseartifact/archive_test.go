package releaseartifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCreateArchiveHasExpectedEntriesAndMetadata(t *testing.T) {
	root := t.TempDir()

	binaryPath := filepath.Join(root, "binary")
	if err := os.WriteFile(binaryPath, []byte("binary-content"), 0o700); err != nil {
		t.Fatalf("write binary fixture: %v", err)
	}
	licensePath := filepath.Join(root, "LICENSE")
	if err := os.WriteFile(licensePath, []byte("license-content"), 0o600); err != nil {
		t.Fatalf("write license fixture: %v", err)
	}
	noticesPath := filepath.Join(root, "THIRD_PARTY_NOTICES")
	if err := os.WriteFile(noticesPath, []byte("third-party-notices"), 0o600); err != nil {
		t.Fatalf("write notices fixture: %v", err)
	}

	outputPath := filepath.Join(root, "pi-worker_v0.1.0_linux_amd64.tar.gz")
	buildDate := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)
	if err := createArchive(binaryPath, licensePath, noticesPath, outputPath, buildDate); err != nil {
		t.Fatalf("createArchive() unexpected error: %v", err)
	}

	entries := readTarEntries(t, outputPath)
	got := make([]string, len(entries))
	for i, entry := range entries {
		got[i] = entry.Name
	}

	expectedNames := []string{"LICENSE", "THIRD_PARTY_NOTICES", "pi-worker"}
	if !reflect.DeepEqual(got, expectedNames) {
		t.Fatalf("archive entry order mismatch\n got: %#v\nwant: %#v", got, expectedNames)
	}

	tests := map[string]struct {
		testMode int64
		testData string
	}{
		"LICENSE":             {testMode: 0o644, testData: "license-content"},
		"THIRD_PARTY_NOTICES": {testMode: 0o644, testData: "third-party-notices"},
		"pi-worker":           {testMode: 0o755, testData: "binary-content"},
	}
	for _, entry := range entries {
		fixture, ok := tests[entry.Name]
		if !ok {
			t.Fatalf("unexpected archive entry %q", entry.Name)
		}
		if entry.Mode != fixture.testMode {
			t.Fatalf("entry %s mode = %o, want %o", entry.Name, entry.Mode, fixture.testMode)
		}
		if entry.Data != fixture.testData {
			t.Fatalf("entry %s data = %q, want %q", entry.Name, entry.Data, fixture.testData)
		}
		if !entry.ModTime.Equal(buildDate) {
			t.Fatalf("entry %s mod time = %s, want %s", entry.Name, entry.ModTime.Format(time.RFC3339), buildDate.Format(time.RFC3339))
		}
		if entry.Uid != 0 || entry.Gid != 0 {
			t.Fatalf("entry %s owner not normalized: uid=%d gid=%d", entry.Name, entry.Uid, entry.Gid)
		}
	}
}

func TestCreateArchiveBytesAndGzipMetadataAreDeterministic(t *testing.T) {
	root := t.TempDir()
	binaryPath := filepath.Join(root, "binary")
	licensePath := filepath.Join(root, "LICENSE")
	noticesPath := filepath.Join(root, "THIRD_PARTY_NOTICES")
	for path, content := range map[string]string{
		binaryPath:  "binary-content",
		licensePath: "license-content",
		noticesPath: "third-party-notices",
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write fixture %s: %v", path, err)
		}
	}

	buildDate := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)
	firstPath := filepath.Join(root, "first.tar.gz")
	secondPath := filepath.Join(root, "second.tar.gz")
	if err := createArchive(binaryPath, licensePath, noticesPath, firstPath, buildDate); err != nil {
		t.Fatalf("create first archive: %v", err)
	}
	if err := createArchive(binaryPath, licensePath, noticesPath, secondPath, buildDate); err != nil {
		t.Fatalf("create second archive: %v", err)
	}

	first, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatalf("read first archive: %v", err)
	}
	second, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatalf("read second archive: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("identical archive inputs produced different tar.gz bytes")
	}

	reader, err := gzip.NewReader(bytes.NewReader(first))
	if err != nil {
		t.Fatalf("read gzip metadata: %v", err)
	}
	if !reader.ModTime.IsZero() || reader.Name != "" || reader.Comment != "" || reader.Extra != nil || reader.OS != 255 {
		t.Fatalf("unstable gzip metadata: modtime=%v name=%q comment=%q extra=%v os=%d", reader.ModTime, reader.Name, reader.Comment, reader.Extra, reader.OS)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close gzip reader: %v", err)
	}
}

func TestCreateArchiveRejectsSymlinkSources(t *testing.T) {
	root := t.TempDir()
	sources := map[string]string{
		"binary":  filepath.Join(root, "binary"),
		"license": filepath.Join(root, "LICENSE"),
		"notice":  filepath.Join(root, "THIRD_PARTY_NOTICES"),
	}
	for name, path := range sources {
		if err := os.WriteFile(path, []byte(name+" target"), 0o644); err != nil {
			t.Fatalf("write %s target: %v", name, err)
		}
	}

	for name, source := range sources {
		t.Run(name, func(t *testing.T) {
			link := source + ".link"
			if err := os.Symlink(source, link); err != nil {
				t.Fatalf("create %s symlink: %v", name, err)
			}
			binaryPath, licensePath, noticePath := sources["binary"], sources["license"], sources["notice"]
			switch name {
			case "binary":
				binaryPath = link
			case "license":
				licensePath = link
			case "notice":
				noticePath = link
			}
			if err := createArchive(binaryPath, licensePath, noticePath, filepath.Join(root, name+".tar.gz"), time.Time{}); err == nil {
				t.Fatalf("createArchive archived symlink source %s", name)
			}
		})
	}
}

func TestCreateArchiveRejectsNonRegularSource(t *testing.T) {
	root := t.TempDir()
	binaryPath := filepath.Join(root, "binary")
	licensePath := filepath.Join(root, "LICENSE")
	noticePath := filepath.Join(root, "THIRD_PARTY_NOTICES")
	for path, content := range map[string]string{
		binaryPath:  "binary-content",
		licensePath: "license-content",
		noticePath:  "third-party-notices",
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write fixture %s: %v", path, err)
		}
	}
	if err := os.Remove(licensePath); err != nil {
		t.Fatalf("remove license fixture: %v", err)
	}
	if err := os.Mkdir(licensePath, 0o755); err != nil {
		t.Fatalf("create non-regular source: %v", err)
	}

	err := createArchive(binaryPath, licensePath, noticePath, filepath.Join(root, "archive.tar.gz"), time.Time{})
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("createArchive() error = %v, want non-regular source error", err)
	}
}

func TestCreateArchiveUsesOpenedSourceMetadata(t *testing.T) {
	root := t.TempDir()
	binaryPath := filepath.Join(root, "binary")
	licensePath := filepath.Join(root, "LICENSE")
	noticePath := filepath.Join(root, "THIRD_PARTY_NOTICES")
	for path, content := range map[string]string{
		binaryPath:  "binary-content",
		licensePath: "original-license-content",
		noticePath:  "third-party-notices",
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write fixture %s: %v", path, err)
		}
	}

	oldOpenSource := openArchiveSource
	openArchiveSource = func(path string) (io.ReadCloser, error) {
		if path == licensePath {
			if err := os.WriteFile(path, []byte("opened"), 0o644); err != nil {
				return nil, err
			}
		}
		return os.Open(path)
	}
	t.Cleanup(func() { openArchiveSource = oldOpenSource })

	outputPath := filepath.Join(root, "archive.tar.gz")
	if err := createArchive(binaryPath, licensePath, noticePath, outputPath, time.Time{}); err != nil {
		t.Fatalf("createArchive() unexpected error: %v", err)
	}
	for _, entry := range readTarEntries(t, outputPath) {
		if entry.Name == "LICENSE" {
			if entry.Size != int64(len("opened")) || entry.Data != "opened" {
				t.Fatalf("LICENSE metadata/content = size %d, data %q; want size %d, data %q", entry.Size, entry.Data, len("opened"), "opened")
			}
			return
		}
	}
	t.Fatal("LICENSE entry not found")
}

func TestCreateArchiveRequiresSourceFiles(t *testing.T) {
	root := t.TempDir()
	if err := createArchive(filepath.Join(root, "missing"), filepath.Join(root, "LICENSE"), filepath.Join(root, "THIRD_PARTY_NOTICES"), filepath.Join(root, "archive.tar.gz"), time.Time{}); err == nil {
		t.Fatal("expected createArchive to fail with missing source files")
	}
}

func TestCreateArchivePreservesWriteAndCloseErrors(t *testing.T) {
	root := t.TempDir()
	binaryPath := filepath.Join(root, "binary")
	licensePath := filepath.Join(root, "LICENSE")
	noticesPath := filepath.Join(root, "THIRD_PARTY_NOTICES")
	for path, content := range map[string]string{
		binaryPath:  "binary-content",
		licensePath: "license-content",
		noticesPath: "third-party-notices",
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write fixture %s: %v", path, err)
		}
	}

	writeErr := errors.New("archive write failed")
	closeErr := errors.New("archive output close failed")
	oldOpenOutput := openArchiveOutput
	openArchiveOutput = func(string) (io.WriteCloser, error) {
		return &failWriteCloser{writeErr: writeErr, closeErr: closeErr}, nil
	}
	t.Cleanup(func() { openArchiveOutput = oldOpenOutput })

	err := createArchive(binaryPath, licensePath, noticesPath, filepath.Join(root, "archive.tar.gz"), time.Time{})
	if !errors.Is(err, writeErr) {
		t.Fatalf("createArchive() error = %v, want primary write error", err)
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("createArchive() error = %v, want output close error", err)
	}
}

func TestCreateArchivePropagatesSourceCloseError(t *testing.T) {
	root := t.TempDir()
	binaryPath := filepath.Join(root, "binary")
	licensePath := filepath.Join(root, "LICENSE")
	noticesPath := filepath.Join(root, "THIRD_PARTY_NOTICES")
	for path, content := range map[string]string{
		binaryPath:  "binary-content",
		licensePath: "license-content",
		noticesPath: "third-party-notices",
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write fixture %s: %v", path, err)
		}
	}

	wantErr := errors.New("source close failed")
	oldOpenSource := openArchiveSource
	openArchiveSource = func(path string) (io.ReadCloser, error) {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		return &closeErrorReader{ReadCloser: file, err: wantErr}, nil
	}
	t.Cleanup(func() { openArchiveSource = oldOpenSource })

	err := createArchive(binaryPath, licensePath, noticesPath, filepath.Join(root, "archive.tar.gz"), time.Time{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("createArchive() error = %v, want source close error", err)
	}
}

func TestCreateArchivePropagatesOutputCloseError(t *testing.T) {
	root := t.TempDir()
	binaryPath := filepath.Join(root, "binary")
	licensePath := filepath.Join(root, "LICENSE")
	noticesPath := filepath.Join(root, "THIRD_PARTY_NOTICES")
	for path, content := range map[string]string{
		binaryPath:  "binary-content",
		licensePath: "license-content",
		noticesPath: "third-party-notices",
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write fixture %s: %v", path, err)
		}
	}

	wantErr := errors.New("output close failed")
	oldOpenOutput := openArchiveOutput
	openArchiveOutput = func(string) (io.WriteCloser, error) {
		return &closeErrorWriter{err: wantErr}, nil
	}
	t.Cleanup(func() { openArchiveOutput = oldOpenOutput })

	err := createArchive(binaryPath, licensePath, noticesPath, filepath.Join(root, "archive.tar.gz"), time.Time{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("createArchive() error = %v, want output close error", err)
	}
}

type closeErrorWriter struct {
	bytes.Buffer
	err error
}

func (w *closeErrorWriter) Close() error {
	return w.err
}

type failWriteCloser struct {
	writeErr error
	closeErr error
}

func (w *failWriteCloser) Write([]byte) (int, error) {
	return 0, w.writeErr
}

func (w *failWriteCloser) Close() error {
	return w.closeErr
}

type closeErrorReader struct {
	io.ReadCloser
	err error
}

func (r *closeErrorReader) Close() error {
	closeErr := r.ReadCloser.Close()
	if closeErr != nil {
		return errors.Join(r.err, closeErr)
	}
	return r.err
}

func (r *closeErrorReader) Stat() (os.FileInfo, error) {
	statter, ok := r.ReadCloser.(interface{ Stat() (os.FileInfo, error) })
	if !ok {
		return nil, errors.New("source does not support stat")
	}
	return statter.Stat()
}

type tarEntry struct {
	Name    string
	Mode    int64
	Uid     int
	Gid     int
	Size    int64
	Data    string
	ModTime time.Time
}

func readTarEntries(t *testing.T, path string) []tarEntry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("read gzip header: %v", err)
	}
	defer gz.Close()

	tw := tar.NewReader(gz)
	var entries []tarEntry
	for {
		hdr, err := tw.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar entry: %v", err)
		}
		buf := strings.Builder{}
		if _, err := io.Copy(&buf, tw); err != nil {
			t.Fatalf("read tar content: %v", err)
		}
		entries = append(entries, tarEntry{
			Name:    filepath.Clean(hdr.Name),
			Mode:    hdr.Mode,
			Uid:     hdr.Uid,
			Gid:     hdr.Gid,
			Size:    hdr.Size,
			Data:    buf.String(),
			ModTime: hdr.ModTime,
		})
	}
	return entries
}

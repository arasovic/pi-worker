package runlog

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// writeRecord writes one hand-built record file into dir: a start line
// carrying the pid the test chose and, when finished, a finish line.
// The pid is chosen by the test, never read back out of a record, so
// the liveness script's answers stay independent of what the record
// carries.
func writeRecord(t *testing.T, dir, runID string, pid int, finished bool) string {
	t.Helper()
	lines := []map[string]any{
		{
			"schemaVersion": schemaVersion,
			"event":         "start",
			"runId":         runID,
			"startedAt":     "2026-08-30T10:15:00Z",
			"workspace":     "/workspace",
			"pid":           pid,
			"tasks":         []any{},
		},
	}
	if finished {
		lines = append(lines, map[string]any{
			"schemaVersion": schemaVersion,
			"event":         "finish",
			"runId":         runID,
			"finishedAt":    "2026-08-30T10:15:30Z",
		})
	}
	var record strings.Builder
	for _, line := range lines {
		data, err := json.Marshal(line)
		if err != nil {
			t.Fatalf("marshal record line: %v", err)
		}
		record.Write(data)
		record.WriteByte('\n')
	}
	path := filepath.Join(dir, runID+".jsonl")
	if err := os.WriteFile(path, []byte(record.String()), 0o600); err != nil {
		t.Fatalf("write record: %v", err)
	}
	return path
}

// readMarker reads the marker document and fails the test on any
// deviation from the documented shape.
func readMarker(t *testing.T, dir string) marker {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, markerFileName))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var m marker
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode marker %q: %v", data, err)
	}
	return m
}

// withPidAlive replaces the liveness seam for the duration of one
// test, scripted like the other seams in this repo.
func withPidAlive(t *testing.T, alive func(int32) (bool, error)) {
	t.Helper()
	original := pidAlive
	pidAlive = alive
	t.Cleanup(func() { pidAlive = original })
}

// TestInterruptedReportsDeadUnfinishedRecordsOnce asserts the scan
// reports exactly the records with no finish line whose process is
// gone — never a finished one, never a still-running one — and that
// the marker written on the way out makes a second scan silent. This
// is the guard the liveness seam protects: with pidAlive answering
// alive for everything, the interrupted record below must vanish from
// the report.
func TestInterruptedReportsDeadUnfinishedRecordsOnce(t *testing.T) {
	withPidAlive(t, func(pid int32) (bool, error) { return pid != 4242, nil })
	dir := t.TempDir()
	deadPath := writeRecord(t, dir, "20260830T101500Z-1", 4242, false)
	writeRecord(t, dir, "20260830T102000Z-2", 4243, true)
	writeRecord(t, dir, "20260830T103000Z-3", 4244, false)

	paths, err := Interrupted(dir)
	if err != nil {
		t.Fatalf("Interrupted: %v", err)
	}
	if want := []string{deadPath}; !slices.Equal(paths, want) {
		t.Fatalf("interrupted = %v, want %v", paths, want)
	}
	// The watermark passed the dead record, then the finished one, then
	// stopped at the still-running one; nothing needed the reported list.
	m := readMarker(t, dir)
	if m.SchemaVersion != markerSchemaVersion || m.Watermark != "20260830T102000Z-2" || len(m.Reported) != 0 {
		t.Fatalf("marker = %#v", m)
	}

	paths, err = Interrupted(dir)
	if err != nil {
		t.Fatalf("Interrupted: %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("second scan interrupted = %v, want none", paths)
	}
}

// TestInterruptedFindsRunsAfterStillRunningOne asserts the scan stops
// the watermark at the first still-running record but keeps scanning,
// so an interrupted run that started after a long-lived one is found —
// and, remembered in the marker's reported list, still only once. This
// is the guard the stop-at-first-live rule protects: stopping the scan
// at the live record would permanently swallow the one after it.
func TestInterruptedFindsRunsAfterStillRunningOne(t *testing.T) {
	withPidAlive(t, func(pid int32) (bool, error) { return pid == 4244, nil })
	dir := t.TempDir()
	writeRecord(t, dir, "20260830T101500Z-1", 4244, false)
	reportedPath := writeRecord(t, dir, "20260830T103000Z-2", 4242, false)

	paths, err := Interrupted(dir)
	if err != nil {
		t.Fatalf("Interrupted: %v", err)
	}
	if want := []string{reportedPath}; !slices.Equal(paths, want) {
		t.Fatalf("interrupted = %v, want %v", paths, want)
	}
	m := readMarker(t, dir)
	if m.Watermark != "" || len(m.Reported) != 1 || m.Reported[0] != "20260830T103000Z-2" {
		t.Fatalf("marker = %#v", m)
	}

	paths, err = Interrupted(dir)
	if err != nil {
		t.Fatalf("Interrupted: %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("second scan interrupted = %v, want none", paths)
	}
	// The promise stays on disk: the watermark never passed the
	// still-running record, so the reported list is what keeps the
	// second scan silent.
	m = readMarker(t, dir)
	if m.Watermark != "" || len(m.Reported) != 1 || m.Reported[0] != "20260830T103000Z-2" {
		t.Fatalf("marker after second scan = %#v", m)
	}
}

// TestInterruptedMarkerShape pins the marker document's exact JSON
// shape — the field names and version the reader itself validates. The
// empty reported list is omitted, so a marker with nothing to remember
// is the two-field document, and a marker that must remember an id
// past a still-running record carries it in reported. This is the
// guard the marker's reported list protects: a reader that ignored the
// marker would warn again on a second scan and fail this test's
// sibling above.
func TestInterruptedMarkerShape(t *testing.T) {
	withPidAlive(t, func(pid int32) (bool, error) { return false, nil })
	dir := t.TempDir()
	writeRecord(t, dir, "20260830T101500Z-1", 4242, false)
	if _, err := Interrupted(dir); err != nil {
		t.Fatalf("Interrupted: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, markerFileName))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if want := "{\"schemaVersion\":1,\"watermark\":\"20260830T101500Z-1\"}\n"; string(data) != want {
		t.Fatalf("marker = %q, want %q", data, want)
	}

	withPidAlive(t, func(pid int32) (bool, error) { return pid == 4244, nil })
	secondDir := t.TempDir()
	writeRecord(t, secondDir, "20260830T101500Z-1", 4244, false)
	writeRecord(t, secondDir, "20260830T103000Z-2", 4242, false)
	if _, err := Interrupted(secondDir); err != nil {
		t.Fatalf("Interrupted: %v", err)
	}
	data, err = os.ReadFile(filepath.Join(secondDir, markerFileName))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	const want = "{\"schemaVersion\":1,\"watermark\":\"\",\"reported\":[\"20260830T103000Z-2\"]}\n"
	if string(data) != want {
		t.Fatalf("marker = %q, want %q", data, want)
	}
}

// TestInterruptedIgnoresMissingOrInvalidMarker asserts a missing,
// unreadable, or wrong-version marker is treated as absent: the scan
// starts from the beginning and the interrupted record is still
// reported even though a stale marker claims the scan is past it. A
// marker that was never there is the first run of every other test;
// here it is a marker that lies.
func TestInterruptedIgnoresMissingOrInvalidMarker(t *testing.T) {
	withPidAlive(t, func(pid int32) (bool, error) { return false, nil })
	for _, test := range []struct {
		name       string
		stale      string
		unreadable bool
	}{
		{name: "not json", stale: "not json at all\n"},
		{name: "wrong version", stale: `{"schemaVersion":2,"watermark":"20260830T103000Z-2"}`},
		// The well-formed variant is unreadable rather than invalid: a
		// marker that cannot be read must behave exactly like one that
		// never existed, no matter what it holds.
		{name: "unreadable", stale: `{"schemaVersion":1,"watermark":"20260830T103000Z-2"}`, unreadable: runtime.GOOS != "windows"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeRecord(t, dir, "20260830T103000Z-2", 4242, false)
			markerPath := filepath.Join(dir, markerFileName)
			if err := os.WriteFile(markerPath, []byte(test.stale), 0o600); err != nil {
				t.Fatalf("write stale marker: %v", err)
			}
			if test.unreadable {
				if err := os.Chmod(markerPath, 0o000); err != nil {
					t.Fatalf("chmod marker: %v", err)
				}
				t.Cleanup(func() { os.Chmod(markerPath, 0o600) })
			}
			paths, err := Interrupted(dir)
			if err != nil {
				t.Fatalf("Interrupted: %v", err)
			}
			if want := []string{path}; !slices.Equal(paths, want) {
				t.Fatalf("interrupted = %v, want %v", paths, want)
			}
		})
	}
}

// TestInterruptedEmptyRecordIsExaminedAgain asserts a zero-length
// record is a record not yet written, never a settled one: the
// writer creates the file and writes the start line in the next
// instant, and a scan that catches the file in between must not let
// the watermark pass it. The first scan skips the empty record and
// the watermark does not move; the run then ends interrupted — the
// record now carries only the start line — and the second scan,
// which would have skipped the record forever had the first one
// settled it, reports it.
func TestInterruptedEmptyRecordIsExaminedAgain(t *testing.T) {
	withPidAlive(t, func(pid int32) (bool, error) { return false, nil })
	dir := t.TempDir()
	path := filepath.Join(dir, "20260830T101500Z-1.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write empty record: %v", err)
	}

	paths, err := Interrupted(dir)
	if err != nil {
		t.Fatalf("Interrupted: %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("interrupted = %v, want none while the record is still empty", paths)
	}
	m := readMarker(t, dir)
	if m.Watermark != "" {
		t.Fatalf("watermark = %q, want it not to have moved past the empty record", m.Watermark)
	}

	// The run ends interrupted: the record holds only the start
	// line, the writer gone.
	writeRecord(t, dir, "20260830T101500Z-1", 4242, false)
	paths, err = Interrupted(dir)
	if err != nil {
		t.Fatalf("Interrupted: %v", err)
	}
	if want := []string{path}; !slices.Equal(paths, want) {
		t.Fatalf("interrupted = %v, want %v", paths, want)
	}
}

// TestInterruptedCorruptRecordIsSettled asserts a record that cannot
// be read or parsed counts as settled: the watermark passes it, so one
// corrupt file can never freeze the scan, and nothing about it is ever
// warned.
func TestInterruptedCorruptRecordIsSettled(t *testing.T) {
	withPidAlive(t, func(pid int32) (bool, error) { return false, nil })
	dir := t.TempDir()
	corrupt := map[string]string{
		"20260830T100000Z-0.jsonl":  "not json at all\n",
		"20260830T100500Z-0a.jsonl": "{\"schemaVersion\":1,\"event\":\"worker\",\"pid\":4242}\n",
		"20260830T101500Z-0b.jsonl": "{\"schemaVersion\":1,\"event\":\"start\"}\n",
	}
	for name, content := range corrupt {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write corrupt record %s: %v", name, err)
		}
	}
	reportedPath := writeRecord(t, dir, "20260830T103000Z-2", 4242, false)

	paths, err := Interrupted(dir)
	if err != nil {
		t.Fatalf("Interrupted: %v", err)
	}
	if want := []string{reportedPath}; !slices.Equal(paths, want) {
		t.Fatalf("interrupted = %v, want %v", paths, want)
	}
	// The corrupt records, the wrong-event record, and the pid-less
	// record all let the watermark pass; the interrupted one after them
	// was still found.
	m := readMarker(t, dir)
	if m.Watermark != "20260830T103000Z-2" {
		t.Fatalf("watermark = %q, want past the corrupt records", m.Watermark)
	}
}

// TestInterruptedLivenessErrorMeansAlive asserts a liveness error is
// treated as alive — no warning, and the watermark stops, exactly as
// for a genuinely still-running record. Doubtful cases resolve toward
// silence.
func TestInterruptedLivenessErrorMeansAlive(t *testing.T) {
	withPidAlive(t, func(pid int32) (bool, error) {
		if pid == 4242 {
			return false, errors.New("liveness unavailable")
		}
		return false, nil
	})
	dir := t.TempDir()
	writeRecord(t, dir, "20260830T101500Z-1", 4242, false)
	reportedPath := writeRecord(t, dir, "20260830T103000Z-2", 4243, false)

	paths, err := Interrupted(dir)
	if err != nil {
		t.Fatalf("Interrupted: %v", err)
	}
	if want := []string{reportedPath}; !slices.Equal(paths, want) {
		t.Fatalf("interrupted = %v, want %v", paths, want)
	}
	m := readMarker(t, dir)
	if m.Watermark != "" || len(m.Reported) != 1 || m.Reported[0] != "20260830T103000Z-2" {
		t.Fatalf("marker = %#v", m)
	}
}

// TestInterruptedMissingRecordsDirIsSilent asserts a records directory
// that does not exist produces no records, no warning, and no marker:
// there is nothing to report and no marker to write.
func TestInterruptedMissingRecordsDirIsSilent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	paths, err := Interrupted(dir)
	if err != nil || len(paths) != 0 {
		t.Fatalf("Interrupted(missing dir) = (%v, %v), want ([], nil)", paths, err)
	}
	if _, err := os.Stat(filepath.Join(dir, markerFileName)); !os.IsNotExist(err) {
		t.Fatalf("marker written into a missing records directory: %v", err)
	}
}

// TestInterruptedUnreadableRecordsDirReturnsError asserts a records
// directory that exists but cannot be read returns the error and no
// paths, so the CLI warns once and continues.
func TestInterruptedUnreadableRecordsDirReturnsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not enforced on Windows")
	}
	dir := t.TempDir()
	writeRecord(t, dir, "20260830T101500Z-1", 4242, false)
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	paths, err := Interrupted(dir)
	if err == nil || len(paths) != 0 {
		t.Fatalf("Interrupted(unreadable dir) = (%v, %v), want (nil, error)", paths, err)
	}
}

// TestInterruptedMarkerWriteFailureReturnsPathsAndError asserts a
// records directory that can be read but cannot take the marker still
// yields the interrupted records, with the error alongside, so the CLI
// warns about the marker and prints the runs anyway.
func TestInterruptedMarkerWriteFailureReturnsPathsAndError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not enforced on Windows")
	}
	withPidAlive(t, func(pid int32) (bool, error) { return false, nil })
	dir := t.TempDir()
	path := writeRecord(t, dir, "20260830T101500Z-1", 4242, false)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	paths, err := Interrupted(dir)
	if err == nil {
		t.Fatalf("Interrupted(no marker write) error = nil, want the marker error")
	}
	if want := []string{path}; !slices.Equal(paths, want) {
		t.Fatalf("interrupted = %v, want %v", paths, want)
	}
}

// TestInterruptedWritesOnlyIntoGivenDir asserts the marker path is
// built from the dir argument only, never derived from the user's
// config directory again: after a scan of a temporary directory, the
// records directory Dir() resolves to — pointed at a temporary home
// inside the test, so the machine's real one is never consulted, let
// alone polluted — holds no marker. This mirrors the CLI's
// TestRunlogDirStaysUnderSystemTemp; a regression here would silently
// pollute the user's real records directory.
func TestInterruptedWritesOnlyIntoGivenDir(t *testing.T) {
	// os.UserConfigDir derives its answer from a different variable
	// per platform; setting all three keeps Dir() under this test's
	// temporary home everywhere. The machine's real records directory
	// is off limits: the first real run writes a marker into it, so
	// consulting it here would fail the test forever after.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AppData", t.TempDir())
	realDir, err := Dir()
	if err != nil {
		t.Skipf("real records directory unavailable: %v", err)
	}
	dir := t.TempDir()
	writeRecord(t, dir, "20260830T101500Z-1", 4242, false)
	if _, err := Interrupted(dir); err != nil {
		t.Fatalf("Interrupted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(realDir, markerFileName)); !os.IsNotExist(err) {
		t.Fatalf("marker appeared in the real records directory %s: %v", realDir, err)
	}
}

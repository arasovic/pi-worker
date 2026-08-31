package runlog

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestParseRecordRefusesSymlinkToRecord asserts a *.jsonl symlink
// pointing at a valid record elsewhere is refused before anything is
// read: the record path is not itself a regular file, so it is not a
// record, and a symlink must not smuggle in a file under a record's
// name. The target is parsed first as the positive control — the
// refusal below is only attributable to the symlink if the target
// alone parses. This is the guard the mutation proof protects: were
// the regular-file check deleted, parseRecord would read through the
// symlink and succeed, and this test would fail fast with a real
// assertion failure, not a hang.
func TestParseRecordRefusesSymlinkToRecord(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Symlink requires privileges on Windows")
	}
	dir := t.TempDir()
	target := writeListRecord(t, dir, "20260830T101500Z-1", 4242, "2026-08-30T10:15:00Z", "/workspace", 1, false, "", "")
	if _, err := parseRecord(target); err != nil {
		t.Fatalf("parseRecord(target) = error %v, want the target to parse", err)
	}
	link := filepath.Join(dir, "20260830T103000Z-9.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := parseRecord(link); err == nil || err.Error() != "record is not a regular file" {
		t.Fatalf("parseRecord(symlink) = %v, want error %q", err, "record is not a regular file")
	}
}

// TestParseRecordRefusesSymlinkToDirectory asserts a *.jsonl symlink
// pointing at a directory is refused too. This is the shape that
// reaches parseRecord in the real readers: all three skip a real
// directory through entry.IsDir(), but os.ReadDir reports a symlink
// as a symlink, so IsDir() is false for it and it flows on to
// parseRecord. A plain directory would never get that far, so a test
// with one would stay green with the guard deleted and prove nothing.
func TestParseRecordRefusesSymlinkToDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Symlink requires privileges on Windows")
	}
	dir := t.TempDir()
	link := filepath.Join(dir, "20260830T101500Z-9.jsonl")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := parseRecord(link); err == nil || err.Error() != "record is not a regular file" {
		t.Fatalf("parseRecord(symlink-to-dir) = %v, want error %q", err, "record is not a regular file")
	}
}

// TestParseRecordRefusesOversizedRecord asserts a record file above
// the size ceiling is refused before it is read. The file is a short
// valid record extended with os.Truncate to 33 MiB — one MiB above
// the 32 MiB ceiling, a literal written into the test, never read out
// of the production constant — which makes a sparse file: nothing
// near 33 MiB is ever written to disk, and the refusal happens on the
// stat, before any read.
func TestParseRecordRefusesOversizedRecord(t *testing.T) {
	dir := t.TempDir()
	path := writeListRecord(t, dir, "20260830T101500Z-1", 4242, "2026-08-30T10:15:00Z", "/workspace", 1, false, "", "")
	// 33 MiB: one MiB above the 32 MiB ceiling.
	const oversized int64 = 33 << 20
	if err := os.Truncate(path, oversized); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, err := parseRecord(path); err == nil || err.Error() != "record is too large" {
		t.Fatalf("parseRecord(oversized) = %v, want error %q", err, "record is too large")
	}
}

// TestLeftoversSkipsWorkerWithNegativeCreateTime asserts a worker
// line whose creation time is negative is never reportable, even when
// the scripted process table holds a live process in that group: a
// negative number is not a creation time, and a creation time that is
// not strictly positive must not switch the age floor off. The record
// is settled by its finish line, and the scripted member is younger
// than -1 in the strict less-than comparison — exactly why the
// negative number was dangerous: with the old `!= 0` filter it passes
// the candidate gate, and then every live process in the group
// qualifies at the age comparison. This is the guard the mutation
// proof protects: reverting leftover.go to `w.createTime != 0` makes
// this test fail.
func TestLeftoversSkipsWorkerWithNegativeCreateTime(t *testing.T) {
	calls := withLiveProcesses(t, []liveProcess{
		{pid: 5011, pgid: 5001, createTime: 100},
	}, nil)
	dir := t.TempDir()
	writeLeftoverRecord(t, dir, "20260830T101500Z-1", 4242, true, workerSpec{pid: 5001, createTime: -1})

	leftovers, err := Leftovers(dir)
	if err != nil {
		t.Fatalf("Leftovers: %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("leftovers = %#v, want none", leftovers)
	}
	if *calls != 0 {
		t.Fatalf("liveProcesses called %d times, want 0", *calls)
	}
}

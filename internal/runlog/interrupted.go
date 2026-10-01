package runlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// markerSchemaVersion is the reported-marker document version. Every
// document in this package versions itself independently — the record
// keeps its own schemaVersion — and the marker does the same.
const markerSchemaVersion = 1

// markerFileName is the one small file the interrupted-run reader keeps
// inside the records directory: how far it has already looked, so an
// interrupted run is warned about exactly once across runs. It is
// deliberately not a record: only *.jsonl files are records, so a
// reported.json can never collide with one.
const markerFileName = "reported.json"

// interruptedRecentWindow is how recently a run id must have been
// allocated for the watermark to stop short of it. A run id is allocated
// before its directory exists — worktree preparation and the start
// handshake happen in between — so a run this new may still have its
// directory about to appear; advancing the watermark to it would skip
// that directory for good once it does.
//
// ponytail: the ceiling is a run whose directory appears more than an
// hour after its id was allocated. The upgrade is creating the run
// directory when the id is allocated, which removes the window entirely.
const interruptedRecentWindow = time.Hour

// interruptedNow is the clock the interrupted-run scan measures a run
// id's age against. It is a package variable so tests pin the window;
// production reads the real clock.
var interruptedNow = time.Now

// pidAlive is the private dependency-injection seam for process
// liveness. Tests replace it with a scripted answer so the records they
// write can carry pids the test itself chose; the production value
// consults the real process table. Liveness is measured, never assumed.
var pidAlive = process.PidExists

// marker is the one-shot marker document. Watermark holds a run id —
// the record file name without .jsonl; ids sort chronologically as
// plain strings because a run id starts with a fixed-width UTC
// timestamp — and is how far the reader has already looked. Reported
// holds the run ids the reader reported although the watermark could
// not pass them, because an older run still in flight holds it back.
type marker struct {
	SchemaVersion int      `json:"schemaVersion"`
	Watermark     string   `json:"watermark"`
	Reported      []string `json:"reported,omitempty"`
}

// Interrupted returns the full paths, oldest first, of the records of
// interrupted runs: records with no finish line whose process is no
// longer alive. The two facts are both required — a run still in
// progress also has no finish line, and warning about a live run would
// be a false alarm — so they are measured, not assumed: the finish
// line is the last non-empty line of the record carrying event
// "finish", and the process is the pid of the first line, the start
// line, paired with that line's creation time when it carries one.
// A run directory <id>/ next to the flat records is classified by its
// snapshot and owner lock instead (see classifyRunDir), and an
// interrupted one is reported by the directory's path.
//
// The reader remembers how far it has already looked in the marker
// file reported.json inside dir, written atomically with the same
// pattern the config store uses. Records at or before the watermark
// are never opened again; a record after it is settled when it carries
// its finish line, still running while its process is alive, and
// interrupted when neither — and is then reported once, because the
// marker is updated before Interrupted returns. A record that exists
// but is still empty is not settled: the writer creates the file and
// writes the start line in the next instant, so the empty state is a
// record not yet written, and the scan skips it without moving the
// watermark — the next scan looks at it again. Interrupted always
// writes the marker after the walk; a marker that cannot be written is
// returned as an error alongside the interrupted records, and only a
// records directory that cannot be read returns an error with no
// records.
//
// Four limits are accepted by design, and each names its own
// failure direction:
//
//   - A record whose start line carries the writer's creation time is
//     not fooled by a reused pid: the pair is the identity, and an
//     unrelated process holding the number reads as dead. A record
//     written before the field existed carries the number alone, and
//     for those a reused pid still looks alive forever and holds the
//     watermark back — the original ceiling, unchanged for those
//     records.
//   - A process that has exited but has not been reaped still reports
//     as alive, with its original creation time, so its record reads
//     as a run still in flight even when it carries the pair. This
//     reader cannot tell it from a live run.
//   - Two runs scanning at the same moment both write the marker; the
//     last write wins and one watermark advance can be lost. The worst
//     outcome is one duplicate warning — the atomic rename means the
//     file is never half-written, so there is no lock and no retry.
//   - A record is parsed and then its process is probed: the two are
//     not one snapshot, and a run that finishes between them is
//     reported as interrupted although its record is now complete.
//     Nothing here locks or re-reads to close the gap — that is the
//     accepted ceiling of reading a file another process is still
//     appending to. The cost is one warning line about a run that had
//     just finished, never an action.
func Interrupted(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		// A missing records directory is not an error: there are no
		// records, hence no interrupted runs and no marker to write.
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	now := interruptedNow()
	// A missing, unreadable, or version-mismatched marker counts as
	// absent: the scan starts from the beginning.
	m, _ := loadMarker(dir)
	watermark := m.Watermark
	reported := make(map[string]bool, len(m.Reported))
	for _, id := range m.Reported {
		reported[id] = true
	}

	var interrupted []string
	advancing := true
	for _, entry := range entries {
		name := entry.Name()
		// Two layouts are records: a run directory named by its run id,
		// and the older flat <id>.jsonl file. Every other entry — the
		// marker file among them — is excluded by this filter and must
		// stay excluded. ReadDir returns names sorted, and '.' sorts
		// before every digit, so the two layouts interleave in run id
		// order and the watermark logic below holds across both.
		var runID string
		switch {
		case entry.IsDir():
			if _, err := ParseRunID(name); err != nil {
				continue
			}
			runID = name
		case strings.HasSuffix(name, ".jsonl"):
			runID = strings.TrimSuffix(name, ".jsonl")
		default:
			continue
		}
		// Records at or before the watermark were already looked at.
		if runID <= watermark {
			continue
		}
		path := filepath.Join(dir, name)
		var state runState
		if entry.IsDir() {
			state = classifyRunDir(path, entry)
		} else {
			state = classifyRecord(path, entry)
		}
		switch state {
		case runPending, runRunning:
			// Neither is settled: a newer run may be, but the watermark
			// cannot pass this one until it is. The scan continues past
			// it, so an interrupted run that started later is still
			// found and reported.
			advancing = false
			continue
		case runSettled:
			if advancing && !runIDWithinWindow(runID, now) {
				watermark = runID
			}
			continue
		}
		if reported[runID] {
			// Already warned about on an earlier run; the marker keeps
			// the promise during the window where an older run still
			// in flight holds the watermark back.
			continue
		}
		interrupted = append(interrupted, path)
		if advancing && !runIDWithinWindow(runID, now) {
			watermark = runID
		} else {
			reported[runID] = true
		}
	}

	// The watermark covers every id at or before it now; keeping those
	// in reported would only grow the marker.
	for id := range reported {
		if id <= watermark {
			delete(reported, id)
		}
	}
	next := marker{SchemaVersion: markerSchemaVersion, Watermark: watermark}
	if len(reported) > 0 {
		next.Reported = make([]string, 0, len(reported))
		for id := range reported {
			next.Reported = append(next.Reported, id)
		}
		sort.Strings(next.Reported)
	}
	if err := writeMarker(dir, next); err != nil {
		// The interrupted runs are still reported: the caller warns
		// about the marker and prints them anyway.
		return interrupted, fmt.Errorf("write %s: %w", markerFileName, err)
	}
	return interrupted, nil
}

// runIDWithinWindow reports whether runID's embedded start time is
// within interruptedRecentWindow of now. A run id is allocated before its
// directory exists, so a run this new may still have its directory about
// to appear; the watermark must not pass it, or the directory would be
// skipped for good. An unparseable id is not a run id — the caller has
// already filtered those out — and is treated as outside the window.
func runIDWithinWindow(runID string, now time.Time) bool {
	started, err := ParseRunID(runID)
	if err != nil {
		return false
	}
	return now.Sub(started) < interruptedRecentWindow
}

// runState is what the interrupted-run scan concludes about one run.
type runState int

const (
	// runPending: the run is not written yet; the scan skips it
	// without moving the watermark and looks at it again next time.
	runPending runState = iota
	// runSettled: the run is over, or cannot be read; the watermark
	// may pass it.
	runSettled
	// runRunning: the run's owner is still alive; it stops the
	// watermark.
	runRunning
	// runInterrupted: the run did not finish and its owner is gone.
	runInterrupted
)

// classifyRecord classifies one flat <id>.jsonl record.
func classifyRecord(path string, entry fs.DirEntry) runState {
	// A zero-length record is a record not yet written: the writer
	// creates the file and writes the start line in the next instant,
	// so the file exists empty for the length of one write. It is not
	// corrupt — it simply has nothing in it yet — so it is skipped
	// without settling: neither the watermark nor the reported list
	// moves, and the next scan looks at it again.
	if info, err := entry.Info(); err == nil && info.Size() == 0 {
		return runPending
	}
	pid, createTime, finished, err := inspectRecord(path)
	if err != nil {
		// A record that cannot be read or parsed counts as settled:
		// the watermark passes it, so one corrupt file can never
		// freeze the scan.
		return runSettled
	}
	if finished {
		return runSettled
	}
	if recordProcessAlive(pid, createTime) {
		// A doubtful case counts as alive — a liveness error, a
		// record without a creation time, a creation-time lookup
		// error.
		return runRunning
	}
	return runInterrupted
}

// snapshotFileName is the state document inside a run directory. Its
// writer lives in internal/background; this package decodes only the
// two facts the scan needs, so it does not import that package.
const snapshotFileName = "snapshot.json"

// UnknownGrace is how long a run that cannot be classified yet may stay
// that way before it is treated as abandoned: a record that has not
// been written for an hour is not being written. runs prune spares an
// unreadable record for this long; the interrupted-run scan stops
// waiting on a run directory without a snapshot after it.
const UnknownGrace = time.Hour

// classifyRunDir classifies one run directory by its snapshot, never
// by its record: the snapshot turns terminal before the record's
// finish line is written, and it is the state runs status reads.
//
//   - No snapshot, or a zero-length one, is a run not written yet: the
//     owner creates the directory and its lock, then creates the
//     snapshot and writes it in the next instants. It stays pending —
//     unless the directory has not changed for UnknownGrace, which only
//     an owner killed between those steps leaves behind; that
//     directory counts as settled, so it cannot hold the watermark or
//     re-warn forever.
//   - A snapshot that cannot be read or decoded, or names no usable
//     supervisor pid, is settled, like a corrupt record.
//   - A terminal snapshot is settled.
//   - Otherwise the owner lock decides (OwnerAlive): running while the
//     owner is alive, interrupted once it is gone.
//
// A snapshot torn by a reader racing its first write is decoded as
// damaged and settled: the accepted cost is one missed warning should
// that very run later be interrupted.
func classifyRunDir(runDir string, entry fs.DirEntry) runState {
	data, err := readRecordFile(filepath.Join(runDir, snapshotFileName))
	if errors.Is(err, fs.ErrNotExist) || (err == nil && len(data) == 0) {
		if info, err := entry.Info(); err == nil && time.Since(info.ModTime()) > UnknownGrace {
			return runSettled
		}
		return runPending
	}
	if err != nil {
		return runSettled
	}
	var snap struct {
		Terminal   bool `json:"terminal"`
		Supervisor struct {
			PID        int   `json:"pid"`
			CreateTime int64 `json:"createTime"`
		} `json:"supervisor"`
	}
	if err := json.Unmarshal(data, &snap); err != nil || snap.Supervisor.PID <= 0 {
		return runSettled
	}
	if snap.Terminal {
		return runSettled
	}
	if OwnerAlive(runDir, snap.Supervisor.PID, snap.Supervisor.CreateTime) {
		return runRunning
	}
	return runInterrupted
}

// inspectRecord reads one record and answers the questions the scan
// asks of it: which process the record belongs to — the pid of the
// start line, the first non-empty line, paired with that line's
// creation time — and whether the record carries its finish line —
// its last non-empty line decodes with event "finish". A record that
// cannot be read or parsed returns an error, so the caller treats it
// as settled; that includes a start line that is not a start line or
// carries no usable pid. It answers through parseRecord, the shared
// parse the list reader uses too: the two readers must classify the
// same record the same way.
func inspectRecord(path string) (pid int, createTime int64, finished bool, err error) {
	rec, err := parseRecord(path)
	if err != nil {
		return 0, 0, false, err
	}
	return rec.pid, rec.createTime, rec.finished, nil
}

// loadMarker reads the marker document through the same guarded open
// as the records: readRecordFile refuses anything that is not a
// regular file and anything above the size ceiling without blocking
// on a named pipe. A missing file, an unreadable one, and a document
// whose schemaVersion is not markerSchemaVersion all report absent:
// the caller scans everything.
func loadMarker(dir string) (marker, bool) {
	data, err := readRecordFile(filepath.Join(dir, markerFileName))
	if err != nil {
		return marker{}, false
	}
	var m marker
	if err := json.Unmarshal(data, &m); err != nil || m.SchemaVersion != markerSchemaVersion {
		return marker{}, false
	}
	return m, true
}

// writeMarker writes the marker document into dir with the atomic
// pattern the config store uses: a temporary file in the same
// directory, owner-only permissions, write, sync, close, rename over
// the destination. A reader in flight can never see a half-written
// marker, which is what makes a concurrent last-write-wins loss cost
// at most one duplicate warning.
func writeMarker(dir string, m marker) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	path := filepath.Join(dir, markerFileName)
	tmp, err := os.CreateTemp(dir, "."+markerFileName+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	remove := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if runtime.GOOS != "windows" {
		// Windows has no Unix permission bits; everywhere else the
		// temporary file must be owner-only, like the config file.
		if err := tmp.Chmod(0o600); err != nil {
			remove()
			return err
		}
	}
	if _, err := tmp.Write(data); err != nil {
		remove()
		return err
	}
	if err := tmp.Sync(); err != nil {
		remove()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

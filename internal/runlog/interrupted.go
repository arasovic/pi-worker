package runlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

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
//
// The reader remembers how far it has already looked in the marker
// file reported.json inside dir, written atomically with the same
// pattern the config store uses. Records at or before the watermark
// are never opened again; a record after it is settled when it carries
// its finish line, still running while its process is alive, and
// interrupted when neither — and is then reported once, because the
// marker is updated before Interrupted returns. Interrupted always
// writes the marker after the walk; a marker that cannot be written is
// returned as an error alongside the interrupted records, and only a
// records directory that cannot be read returns an error with no
// records.
//
// Three limits are accepted by design, and all resolve toward
// silence, never toward a wrong accusation:
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
		// Only *.jsonl files are records; the marker file itself is
		// excluded by the same filter and must stay excluded.
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		runID := strings.TrimSuffix(name, ".jsonl")
		// Records at or before the watermark were already looked at.
		if runID <= watermark {
			continue
		}
		pid, createTime, finished, err := inspectRecord(filepath.Join(dir, name))
		if err != nil {
			// A record that cannot be read or parsed counts as settled:
			// the watermark passes it, so one corrupt file can never
			// freeze the scan.
			if advancing {
				watermark = runID
			}
			continue
		}
		if finished {
			if advancing {
				watermark = runID
			}
			continue
		}
		if recordProcessAlive(pid, createTime) {
			// A doubtful case counts as alive — a liveness error, a
			// record without a creation time, a creation-time lookup
			// error — and a still-running record stops the watermark:
			// it may still finish. The scan continues past it, so an
			// interrupted run that started later is still found and
			// reported.
			advancing = false
			continue
		}
		if reported[runID] {
			// Already warned about on an earlier run; the marker keeps
			// the promise during the window where an older run still
			// in flight holds the watermark back.
			continue
		}
		interrupted = append(interrupted, filepath.Join(dir, name))
		if advancing {
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

// loadMarker reads the marker document. A missing file, an unreadable
// one, and a document whose schemaVersion is not markerSchemaVersion
// all report absent: the caller scans everything.
func loadMarker(dir string) (marker, bool) {
	data, err := os.ReadFile(filepath.Join(dir, markerFileName))
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

package runlog

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Leftover is the report of one settled run whose recorded worker
// groups still hold live members: the run's id, its record's path, and
// the pids of the leftover processes, ascending with duplicates
// removed.
type Leftover struct {
	RunID string `json:"runId"`
	Path  string `json:"path"`
	PIDs  []int  `json:"pids"`
}

// workerFacts is the process identity of one worker line of a record:
// the pid the worker launched paired with that process's creation
// time. The pair is the identity — a pid alone is reused, so it cannot
// name a process on its own — and the creation time doubles as the
// group's age floor: nothing older than the worker can be its leftover.
type workerFacts struct {
	pid        int
	createTime int64
}

// liveProcesses is the private dependency-injection seam for the
// process-table sweep. Tests replace it with a scripted table so the
// records they write stay hermetic; the production value consults the
// real process table once per call. Leftovers only ever calls it after
// a settled candidate exists — the sweep is lazy.
var liveProcesses = defaultLiveProcesses

// liveProcess is one entry in the process-table snapshot. An unreadable
// entry carries only its pid; its other fields are not evidence.
type liveProcess struct {
	pid        int
	pgid       int
	createTime int64
	unreadable bool
}

// Leftovers returns one entry per settled run whose recorded worker
// groups still hold live members: processes that ran under the run and
// are still running although nothing the run started is supervised
// anymore. The recorded worker pid is the identity of the worker
// process, which leads its own process group on Unix, so the group
// number equals the pid; a child inherits that group and keeps it when
// it is reparented. The question asked of the process table is
// therefore: which live processes carry one of the record's worker
// pids as their group number, and are no older than the worker that
// started them?
//
// A record is settled exactly when the interrupted-run reader
// considers it over — it carries its finish line, or the process that
// wrote it is no longer the process it was — the start line's pid
// paired with its creation time — measured through the same liveness
// seam. A run still in flight has not left anything behind. A worker
// line without a creation time is never reportable: the identity pair
// is incomplete, so the number alone cannot name a group. And when a
// recorded pid is itself still alive it must still be the same
// process — its creation time must equal the recorded one — or the
// number has been reused by an unrelated process, and nothing in that
// group is attributable to the run.
//
// The process table is swept at most once per call and only when a
// settled candidate exists; the per-record loop is pure map lookups
// over that one snapshot. Every uncertainty resolves toward silence:
// a record that cannot be read or parsed, a row whose group or
// creation time cannot be read, and a sweep that fails entirely all
// report nothing, and only a records directory that cannot be read is
// an error worth returning.
//
// One ceiling is accepted by design, and its failure direction is
// safe: if a worker's group empties completely, the pid wraps all the
// way around, the new holder makes itself the leader of a new group,
// and its members are younger than the recorded creation time, a clean
// run reads as having left processes behind. The cost is one wrong
// line of text and never a signal — leftover pids are reported, never
// killed or otherwise acted on.
//
// A second limit is accepted by design, in the same shape: when a
// recorded worker pid is absent from the process table, the worker is
// treated as exited and the other members of its group are reported.
// The sweep cannot tell that case apart from one where the worker's
// number was reused by an unrelated process that led its own group
// and has since died, leaving its own children carrying the same
// group number — the identity pair cannot help, because a dead leader
// leaves no creation time to compare against, and the orphans carry
// the same group number either way. The cost is one warning line
// naming someone else's processes, never an action: this reader only
// ever reports.
//
// A leftover is a condition that is still true, not an event that
// happened once, so the report is answered fresh on every call: no
// watermark is read or written, and this reader never touches
// reported.json.
func Leftovers(dir string) ([]Leftover, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		// A missing records directory is not an error: there are no
		// records, hence no runs and no leftovers to find.
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	// Only *.jsonl files are records; reported.json, its .tmp-*
	// stages, and any other file are skipped silently, exactly as the
	// other readers skip them.
	type candidate struct {
		runID   string
		path    string
		workers []workerFacts
	}
	var candidates []candidate
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		path := filepath.Join(dir, name)
		rec, err := parseRecord(path)
		if err != nil {
			// A record that cannot be read or parsed is skipped: it
			// cannot be attributed to anything.
			continue
		}
		if !isSettled(rec) {
			// A run still in flight has not left anything behind.
			continue
		}
		workers := make([]workerFacts, 0, len(rec.workers))
		for _, w := range rec.workers {
			// A missing creation time means this worker is not
			// reportable, without exception: the identity pair is
			// incomplete, and the group number alone cannot be
			// attributed to the run. Every record written before the
			// field existed is exactly this class.
			if w.createTime > 0 {
				workers = append(workers, w)
			}
		}
		if len(workers) == 0 {
			continue
		}
		candidates = append(candidates, candidate{
			runID:   strings.TrimSuffix(name, ".jsonl"),
			path:    path,
			workers: workers,
		})
	}
	// The sweep is lazy: no settled record with a reportable worker
	// means the process table never has to be read.
	if len(candidates) == 0 {
		return nil, nil
	}
	rows, err := liveProcesses()
	if err != nil {
		// A process table that cannot be read yields no leftovers:
		// only a records directory that cannot be read is an error
		// worth returning.
		return nil, nil
	}
	// The "not in the future" instant is taken after the process
	// table has been read, never before it: the read takes real
	// time, and every entry in the snapshot was created before this
	// instant, so a process that started during the read has a
	// legitimate creation time earlier than the line. Taken first,
	// the line would fall before such an entry and throw it out as
	// impossible.
	now := time.Now().UnixMilli()
	// One snapshot, indexed both ways, so the per-record loop below is
	// pure map lookups: group number to member pids, pid to row.
	byGroup := make(map[int][]int, len(rows))
	byPID := make(map[int]liveProcess, len(rows))
	unreadablePIDs := make(map[int]bool)
	for _, row := range rows {
		// An entry that is not evidence — one that could not be read
		// at all, or one whose creation time was read but is unusable
		// — leaves the pid's identity unconfirmed, so it is recorded
		// as unreadable and kept out of both indexes: a recorded
		// worker holding such an entry is skipped whole, never looked
		// absent.
		if row.unreadable || !usableCreateTime(row.createTime, now) {
			unreadablePIDs[row.pid] = true
			continue
		}
		byGroup[row.pgid] = append(byGroup[row.pgid], row.pid)
		byPID[row.pid] = row
	}
	var leftovers []Leftover
	for _, candidate := range candidates {
		// An unreadable row for a recorded worker leaves its identity
		// unconfirmed, so this whole record is skipped.
		unreadableWorker := false
		for _, w := range candidate.workers {
			if unreadablePIDs[w.pid] {
				unreadableWorker = true
				break
			}
		}
		if unreadableWorker {
			continue
		}
		seen := make(map[int]bool)
		var pids []int
		for _, w := range candidate.workers {
			// The recorded pid doubles as the group number. When that
			// number is still alive it must still be the same
			// process: a different creation time means the number was
			// reused by an unrelated process, whose group is not this
			// worker's to claim.
			if row, ok := byPID[w.pid]; ok && row.createTime != w.createTime {
				continue
			}
			if !usableCreateTime(w.createTime, now) {
				continue
			}
			for _, pid := range byGroup[w.pid] {
				row := byPID[pid]
				// A process that already existed before the worker
				// started was not started by it.
				if row.createTime < w.createTime || seen[pid] {
					continue
				}
				seen[pid] = true
				pids = append(pids, pid)
			}
		}
		if len(pids) == 0 {
			// A run with no leftovers is absent, never present with
			// an empty pid list.
			continue
		}
		sort.Ints(pids)
		leftovers = append(leftovers, Leftover{RunID: candidate.runID, Path: candidate.path, PIDs: pids})
	}
	return leftovers, nil
}

// isSettled reports whether a run is over, measured exactly as the
// interrupted-run reader measures it: the record carries its finish
// line, or the process that wrote the record — the start line's pid
// paired with its creation time — is no longer the same process, a
// reused number reading as dead because the pair does not match.
// Every doubtful lookup counts as alive, so the run counts as still
// going and is skipped: uncertainty resolves toward silence, never
// toward reporting a live run's processes as leftover.
func isSettled(rec recordFacts) bool {
	if rec.finished {
		return true
	}
	return !recordProcessAlive(rec.pid, rec.createTime)
}

// usableCreateTime is the one rule that decides whether a creation
// time is evidence at all, applied to recorded workers, process-table
// rows, and both sides of recordProcessAlive's comparison: the
// Unix-second component must be positive — no real process was
// created inside the first second of the epoch, and a sub-second
// value such as 1 must not be estimated into a machine uptime — and
// the value must be no later than the instant the caller measured.
// Either side failing it leaves that comparison unreported,
// resolving toward alive.
func usableCreateTime(createTime, now int64) bool {
	return time.UnixMilli(createTime).Unix() > 0 && createTime <= now
}

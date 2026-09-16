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
// pids as their group number, were no older than the worker that
// started them, and were no newer than the run itself? The last of
// the three is the ceiling: a genuine survivor was started by the
// run, so it cannot have been created after the record's finish
// line. A record that carries a finish line whose finishedAt parses
// therefore also rejects a group member created after the end of the
// recorded second — the field's one-second resolution would otherwise
// drop a survivor created in the truncated fraction of that second,
// so the ceiling is the last instant of the recorded second, never
// the instant it names literally. A record that offers no instant —
// it has no finish line, or its finishedAt is absent or unparseable
// — offers no ceiling and keeps the age test's floor alone; it does
// not borrow the file's modification time, which is a decision, not
// an oversight. Every uncertainty about the ceiling resolves toward
// silence: a missing or damaged instant leaves the member admitted,
// never dropped on a guess.
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
// number has been reused by an unrelated process, and whether the old
// group number still names a live group is decided by the pgid the
// current holder carries, below.
//
// The process table is swept at most once per call and only when a
// settled candidate exists; the per-record loop is pure map lookups
// over that one snapshot. Every uncertainty resolves toward silence:
// a record that cannot be read or parsed, a row whose group or
// creation time cannot be read, and a sweep that fails entirely all
// report nothing, and only a records directory that cannot be read is
// an error worth returning.
//
// A recorded pid that is still alive must still be the same process —
// its creation time must equal the recorded one — or the number has
// been reused by an unrelated process, and the current holder's pgid
// decides what the reuse means:
//
//  1. The holder leads the group the recorded number names — its pgid
//     is its own pid, which is the recorded number. The number now
//     names the holder's own group, so its members are the holder's
//     own descendants, not the run's, and nothing is reported.
//  2. The holder does not lead that group — its pgid is not the
//     recorded number, so it belongs to some other group. The
//     recorded number's group, if any live process still carries it,
//     holds the recorded worker's genuine survivors, which are
//     inspected under the existing age floor and the finish ceiling.
//
// A pid equal to its own pgid is the leader of that group, and only a
// leader's number can name a group it owns; a pid that is not its own
// pgid belongs to whatever group its pgid names.
//
// A recorded worker pid absent from the process table — the worker
// exited, or its number was reused by a process that has since died —
// leaves no holder to compare, and the group's members are inspected
// the same way. The reader cannot tell a genuine survivor of the run
// from a child of a dead leader that happened to take the number,
// lead a group of its own, and die before the sweep: no live holder
// remains to reveal the reuse. The finish ceiling is what narrows
// this class — a member of the reused number's group created after
// the recorded run ended is not the run's, exactly the shape issue
// #305 measured, where the warning named a process started six days
// after its run — but a member created before the finish instant
// still cannot be attributed with certainty, so one wrong line of
// text remains possible in that narrower window. The product never
// acts on the numbers it reports.
//
// A record whose worker line carries no creation time is never
// reportable, whatever the process table holds: the identity pair is
// incomplete, so the group number alone cannot be attributed to the
// run. Every record written before the worker createTime field
// existed is exactly this class — old records stay unreportable by
// design, and this limit is accepted rather than guessed around.
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
		// finishCeilingMillis is the last instant at which a member
		// of this run's groups can still have been started by the
		// run: the last millisecond of the whole second the record's
		// finishedAt names. Zero means the record offers no instant
		// — it has no finish line, or its finishedAt is absent or
		// unparseable — and the age test keeps its floor alone.
		finishCeilingMillis int64
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
			// The finish line is written at one-second resolution, so
			// finishedAt names a whole second, not the exact instant
			// the run ended: a run that really ended at 23:01:22.900
			// records 23:01:22. A genuine survivor created at
			// 23:01:22.500 — before the run ended — would sit after a
			// ceiling taken literally, so the ceiling is the last
			// instant of the recorded second, and a member created
			// anywhere inside that second is still the run's. A record
			// with no finish line offers no instant and keeps today's
			// floor-only behaviour: this reader does not fall back to
			// the file's modification time, which is a decision, not an
			// oversight.
			finishCeilingMillis: wholeSecondEnd(rec.finishedAtMillis),
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
		// worker holding such an entry is skipped, never looked
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
		seen := make(map[int]bool)
		var pids []int
		for _, w := range candidate.workers {
			// A worker whose own row cannot be read has an unconfirmed
			// identity, so that worker is skipped — but the skip is per
			// worker, never per record: one unreadable row can no
			// longer silence the other workers of the same run.
			if unreadablePIDs[w.pid] {
				continue
			}
			if !usableCreateTime(w.createTime, now) {
				continue
			}
			// The recorded pid doubles as the group number. When that
			// number is still alive, the current holder decides what the
			// number means: a holder that leads the group the number
			// names — its pgid equals its pid, the recorded number — is
			// either the recorded worker still alive (its creation time
			// matches) or an unrelated process that took the number
			// over and leads a group of its own under it. A holder that
			// does not lead that group belongs to some other group, so
			// the number's own group is not the holder's and is
			// inspected as the old group still holding its survivors.
			if row, ok := byPID[w.pid]; ok && row.pid == row.pgid {
				if row.createTime != w.createTime {
					// The number was reused by an unrelated process
					// that leads a fresh group of its own under it: the
					// old group number is now this new group's number,
					// and its members are the new holder's own, not the
					// run's.
					continue
				}
				// Still the same worker: its group is genuinely the
				// worker's, and its members are inspected.
			}
			// The recorded number is not alive, is alive but does not
			// lead the group it names — the old group still holds its
			// survivors — or is the same worker still alive. All three
			// leave the group number live, so its members are inspected
			// under the existing age floor and the finish ceiling.
			for _, pid := range byGroup[w.pid] {
				row := byPID[pid]
				// A process that already existed before the worker
				// started was not started by it, and a process created
				// after the run ended was not started by the run either.
				// The ceiling is zero for a record with no usable finish
				// instant, which turns the upper test off; every
				// uncertainty about the ceiling resolves toward silence,
				// never toward dropping a member on a guess.
				if row.createTime < w.createTime || seen[pid] ||
					(candidate.finishCeilingMillis != 0 && row.createTime > candidate.finishCeilingMillis) {
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

// wholeSecondEnd returns the last instant of the whole second the
// given millisecond instant falls in, or zero for zero. The finish
// line records finishedAt at one-second resolution, so the instant it
// names is a whole second, and a genuine survivor of the run may have
// been created anywhere inside that same second — after the truncated
// instant but before the run's true, sub-second end. Extending the
// ceiling to the end of the recorded second admits those members
// while still rejecting anything created in a later second; a run
// that ended at 23:01:22.900 records 23:01:22, and the ceiling is
// 23:01:22.999. Zero passes through unchanged: it is the "no finish
// instant" sentinel, never a real instant to widen.
func wholeSecondEnd(millis int64) int64 {
	if millis == 0 {
		return 0
	}
	return time.UnixMilli(millis).Truncate(time.Second).Add(time.Second - time.Millisecond).UnixMilli()
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

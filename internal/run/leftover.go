package run

import (
	"bytes"
	"encoding/binary"
	"sort"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// RunMarkerEnv is the environment variable the controller puts into
// pi-worker's own process environment before any worker starts. Every
// process a run starts inherits it through normal inheritance, so a
// live process that still carries it after the run ended belongs to
// that run.
const RunMarkerEnv = "PI_WORKER_RUN"

// LeftoverProcess is a live process that still carries this run's
// marker after the run ended. It is reported, never ended; Name is the
// program name only, never its arguments.
type LeftoverProcess struct {
	PID  int    `json:"pid"`
	Name string `json:"name,omitempty"`
}

// hasRunMarker reports whether env carries exactly RunMarkerEnv=runID.
// The match is on the whole entry: a value that merely contains the
// marker, or shares a prefix with runID, does not count.
func hasRunMarker(env []string, runID string) bool {
	want := RunMarkerEnv + "=" + runID
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}

// procargsEnvironment parses a kern.procargs2 buffer and returns only
// the environment entries. Layout: a 4-byte little-endian argc, then
// the exec path, NUL padding, argc argument strings each NUL-terminated,
// then the environment strings each NUL-terminated, then trailing NULs.
//
// The buffer is split on NUL the same way gopsutil's parseCmdline does.
// The exec path and the empty padding chunks up to the first non-empty
// chunk — argument 0 — are skipped, then exactly argc chunks are
// skipped counting empty ones: an empty argument must not shift an
// argument into the environment. The remaining chunks up to the first
// empty one are the environment.
//
// A buffer shorter than 4 bytes, or one that ends before argc arguments
// were read, returns nil. It never panics on any input.
func procargsEnvironment(buf []byte) []string {
	if len(buf) < 4 {
		return nil
	}
	argc := int64(binary.LittleEndian.Uint32(buf[:4]))
	chunks := bytes.Split(buf[4:], []byte{0})
	// chunks[0] is the exec path; skip it and any padding NULs before
	// argv[0], the first non-empty chunk.
	i := 1
	for ; i < len(chunks) && len(chunks[i]) == 0; i++ {
	}
	// Skip exactly argc argument chunks, counting empty ones.
	if argc < 0 || argc > int64(len(chunks)-i) {
		return nil
	}
	i += int(argc)
	// The environment starts here and ends at the first empty chunk,
	// which is the first of the trailing NULs.
	var env []string
	for ; i < len(chunks) && len(chunks[i]) > 0; i++ {
		env = append(env, string(chunks[i]))
	}
	return env
}

// processEnviron reads one process's environment. It is a seam for
// tests; the real implementation is per-OS.
var processEnviron = defaultProcessEnviron

// runProcesses finds live processes carrying runID's marker. It is a
// seam for tests.
var runProcesses = findRunProcesses

// findRunProcesses lists the live processes and returns the ones that
// were created at or after since and still carry runID's marker, sorted
// by pid ascending. A process created before since is skipped before
// its environment is read, because it cannot carry this run's marker.
// It returns nil when listing fails or nothing matches, and it never
// returns an error: a lookup problem must never fail a run.
func findRunProcesses(runID string, since time.Time) []LeftoverProcess {
	procs, err := process.Processes()
	if err != nil {
		return nil
	}
	cutoff := since.Truncate(time.Second).UnixMilli()
	var found []LeftoverProcess
	for _, p := range procs {
		created, err := p.CreateTime()
		if err != nil || created < cutoff {
			continue
		}
		env, err := processEnviron(p.Pid)
		if err != nil {
			continue
		}
		if !hasRunMarker(env, runID) {
			continue
		}
		name, err := p.Name()
		if err != nil {
			name = ""
		}
		found = append(found, LeftoverProcess{PID: int(p.Pid), Name: name})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].PID < found[j].PID })
	return found
}

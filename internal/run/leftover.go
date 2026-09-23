package run

import (
	"bytes"
	"encoding/binary"
	"path/filepath"
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
// base name of the program file, or the process name when the file
// cannot be read, and never its arguments.
type LeftoverProcess struct {
	PID  int    `json:"pid"`
	Name string `json:"name,omitempty"`
}

// leftoverName picks a display name for a leftover process. The
// program file names the process; the process name is only a fallback,
// because on Linux it is the main thread's name, which a program can
// change (node reports "MainThread").
func leftoverName(exe string, exeErr error, name string, nameErr error) string {
	if exeErr == nil && exe != "" {
		return filepath.Base(exe)
	}
	if nameErr == nil {
		return name
	}
	return ""
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
// the environment entries. Layout, measured on macOS: a 4-byte
// little-endian argc, then the exec path, its NUL terminator, NUL
// padding up to roundup(len(path)+1, 8) bytes counted from offset 4,
// then argc argument strings each NUL-terminated, then the environment
// strings each NUL-terminated, then trailing NULs. The path, its NUL,
// and the padding always occupy that rounded-up size, so argv[0] starts
// at buffer offset 4 + roundup(len(path)+1, 8) even when argv[0] is the
// empty string.
//
// The buffer is split on NUL. Exactly argc chunks are skipped from
// argv[0], counting empty ones: an empty argument must not shift an
// argument into the environment. The remaining chunks up to the first
// empty one are the environment.
//
// A buffer shorter than 4 bytes, one without a NUL after the argc, one
// that ends before argv[0] starts, one with a non-NUL byte in the
// padding, or one that ends before argc arguments were read, returns
// nil. An unrecognised layout reports nothing rather than guessing, so
// it can never produce a false match. It never panics on any input.
func procargsEnvironment(buf []byte) []string {
	if len(buf) < 4 {
		return nil
	}
	argc := int64(binary.LittleEndian.Uint32(buf[:4]))
	// The exec path ends at the first NUL at or after offset 4.
	pathEnd := bytes.IndexByte(buf[4:], 0)
	if pathEnd < 0 {
		return nil
	}
	pathEnd += 4
	// argv[0] starts after the path's NUL and its NUL padding, rounded
	// up to an 8-byte boundary counted from offset 4.
	argvStart := 4 + roundup(pathEnd-4+1, 8)
	if argvStart > len(buf) {
		return nil
	}
	// Every byte between the path's NUL and argv[0] is padding NUL.
	for _, b := range buf[pathEnd:argvStart] {
		if b != 0 {
			return nil
		}
	}
	chunks := bytes.Split(buf[argvStart:], []byte{0})
	// Skip exactly argc argument chunks, counting empty ones.
	if argc < 0 || argc > int64(len(chunks)) {
		return nil
	}
	i := int(argc)
	// The environment starts here and ends at the first empty chunk,
	// which is the first of the trailing NULs.
	var env []string
	for ; i < len(chunks) && len(chunks[i]) > 0; i++ {
		env = append(env, string(chunks[i]))
	}
	return env
}

// roundup returns n rounded up to the next multiple of multiple.
func roundup(n, multiple int) int {
	return (n + multiple - 1) / multiple * multiple
}

// processEnviron reads one process's environment. It is a seam for
// tests; the real implementation is per-OS.
var processEnviron = defaultProcessEnviron

// runProcesses finds live processes carrying runID's marker. It is a
// seam for tests.
var runProcesses = findRunProcesses

// createdFloor returns the earliest creation time, in Unix
// milliseconds, that can belong to a process started at or after since.
// On Linux a creation time is derived from a boot time counted in whole
// seconds, so it can read up to a second early; the two-second margin
// keeps a process started just after since from being skipped, and costs
// only a few extra reads.
func createdFloor(since time.Time) int64 {
	return since.Truncate(time.Second).Add(-2 * time.Second).UnixMilli()
}

// findRunProcesses lists the live processes and returns the ones that
// were created at or after since and still carry runID's marker, sorted
// by pid ascending. A process created more than about two seconds before
// since is skipped before its environment is read, because it cannot
// carry this run's marker. It returns nil when listing fails or nothing
// matches, and it never returns an error: a lookup problem must never
// fail a run.
func findRunProcesses(runID string, since time.Time) []LeftoverProcess {
	procs, err := process.Processes()
	if err != nil {
		return nil
	}
	cutoff := createdFloor(since)
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
		exe, exeErr := p.Exe()
		name, nameErr := p.Name()
		found = append(found, LeftoverProcess{PID: int(p.Pid), Name: leftoverName(exe, exeErr, name, nameErr)})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].PID < found[j].PID })
	return found
}

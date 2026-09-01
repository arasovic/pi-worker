//go:build !darwin && !linux

package runlog

// defaultLiveProcesses reports no processes where the process group
// cannot be read: the leftover reader is silently absent there. The
// package still builds and every Leftovers call still answers — with
// nothing — the same way the worker still builds and runs with the
// same lifecycle where process-tree containment is unsupported; only
// the leftover sweep capability is absent.
func defaultLiveProcesses() ([]liveProcess, error) {
	return nil, nil
}

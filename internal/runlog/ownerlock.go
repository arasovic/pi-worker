package runlog

// OwnerLockName is the file inside a run directory whose advisory lock
// the process that owns the run holds for as long as it lives. The
// kernel drops the lock when that process exits, however it exits, so
// the lock answers "is the owner still there" without a pid lookup.
const OwnerLockName = "owner.lock"

// LockState is what one probe of a run directory's owner lock found.
type LockState int

const (
	// LockUnknown: the probe could not tell — the file could not be
	// opened or locked for a reason other than absence or contention.
	// Readers treat it as alive.
	LockUnknown LockState = iota
	// LockHeld: another process holds the owner lock.
	LockHeld
	// LockFree: the lock file exists and nobody holds it; the owner
	// is gone.
	LockFree
	// LockAbsent: the run directory has no owner lock file.
	LockAbsent
)

// OwnerAlive reports whether the process owning the run in runDir is
// still there. When the run directory has an owner lock file, the lock
// alone decides: held or unknown is alive, free is dead. Without one —
// a run written before the lock existed — the recorded pid and
// creation time decide, by the same rule the record readers use.
func OwnerAlive(runDir string, pid int, createTime int64) bool {
	switch ProbeOwnerLock(runDir) {
	case LockFree:
		return false
	case LockAbsent:
		return recordProcessAlive(pid, createTime)
	default:
		return true
	}
}

//go:build !darwin && !linux

package background

// SupportsBackgroundRuns reports that no background run can exist on this
// platform: the supervisor and worker-host role processes a background run is
// executed by cannot start here, which is why Start refuses the run with
// errRoleProcessUnsupported. A caller that only reads a run's state — a status
// or a wait — asks here, so it refuses where nothing could ever have been
// started instead of reporting a run it never had.
func SupportsBackgroundRuns() bool { return false }

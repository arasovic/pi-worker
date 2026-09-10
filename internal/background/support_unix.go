//go:build darwin || linux

package background

// SupportsBackgroundRuns reports whether a background run can exist on this
// platform: it can where the supervisor and worker-host role processes a
// background run is executed by can start, which is where Start can hand a
// run over to one. A caller that only reads a run's state — a status or a
// wait — asks here, so it refuses where nothing could ever have been started
// instead of reporting a run it never had.
func SupportsBackgroundRuns() bool { return true }

//go:build !darwin && !linux

package pi

// ProcessIdentity names a process by pid plus its creation time. The pair is
// the identity — a pid alone is reused.
type ProcessIdentity struct {
	PID        int
	CreateTime int64
}

// LiveDescendants is the non-Unix stub: descendant lookup is not implemented
// on this platform, so it reports no descendants. The recorder's sweeper
// treats a nil result as "nothing found" and keeps running harmlessly.
func LiveDescendants(roots []ProcessIdentity) [][]ProcessIdentity {
	return nil
}

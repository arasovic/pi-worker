//go:build darwin || linux

package admission

import "testing"

// TestGateOwnerIdentityPositiveStable proves the projection returns the
// positive identity sampled at Open and is stable across repeated calls.
func TestGateOwnerIdentityPositiveStable(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 1, withOwnerPID(4242, 4242000))

	first := g.OwnerIdentity()
	if first.PID != 4242 {
		t.Errorf("OwnerIdentity().PID = %d, want 4242", first.PID)
	}
	if first.CreateTime != 4242000 {
		t.Errorf("OwnerIdentity().CreateTime = %d, want 4242000", first.CreateTime)
	}
	if first.PID <= 0 || first.CreateTime <= 0 {
		t.Errorf("OwnerIdentity() = %+v, want positive PID and CreateTime", first)
	}
	if second := g.OwnerIdentity(); second != first {
		t.Errorf("OwnerIdentity() not stable: first=%+v second=%+v", first, second)
	}
}

// TestGateOwnerIdentityMatchesStoredOwner proves the exported projection
// equals the Gate's stored private owner identity field by field.
func TestGateOwnerIdentityMatchesStoredOwner(t *testing.T) {
	root := t.TempDir()
	g, _ := openGateForTest(t, root, 1, withOwnerPID(31337, 31337000))

	id := g.OwnerIdentity()
	if id.PID != g.owner.PID || id.CreateTime != g.owner.CreateTime {
		t.Errorf("OwnerIdentity() = {%d, %d}, want Gate stored owner {%d, %d}",
			id.PID, id.CreateTime, g.owner.PID, g.owner.CreateTime)
	}
}

// TestNilGateOwnerIdentityZeroValue proves a nil *Gate projects the zero
// OwnerIdentity without panicking.
func TestNilGateOwnerIdentityZeroValue(t *testing.T) {
	var g *Gate
	got := g.OwnerIdentity()
	if got != (OwnerIdentity{}) {
		t.Errorf("nil Gate OwnerIdentity() = %+v, want zero value", got)
	}
	if got.PID != 0 || got.CreateTime != 0 {
		t.Errorf("nil Gate OwnerIdentity() = {%d, %d}, want zero PID and CreateTime",
			got.PID, got.CreateTime)
	}
}

// TestGateOwnerIdentityNoResampling proves the projection reads the
// identity stored by Open and never queries the process table again:
// after the process-time seams change, the projection still returns the
// originally captured identity.
func TestGateOwnerIdentityNoResampling(t *testing.T) {
	root := t.TempDir()
	g, stored := openGateForTest(t, root, 1, withOwnerPID(5150, 5150000))

	before := g.OwnerIdentity()

	// After Open, the process-time seam changes: the PID seam moves to a
	// different process and the creation-time lookup fails outright. Any
	// resampling would fail or report a different identity.
	withOwnerGetpid(t, 9090)
	withPidCreateTime(t, func(_ int) (int64, error) { return 0, errTestSentinel })

	after := g.OwnerIdentity()
	if after != before {
		t.Errorf("OwnerIdentity() after process-time seam change = %+v, want %+v (must not resample)",
			after, before)
	}
	if after.PID != stored.PID || after.CreateTime != stored.CreateTime {
		t.Errorf("OwnerIdentity() after process-time seam change = {%d, %d}, want stored {%d, %d}",
			after.PID, after.CreateTime, stored.PID, stored.CreateTime)
	}
}

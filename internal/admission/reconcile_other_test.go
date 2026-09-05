//go:build !darwin && !linux

package admission

import (
	"errors"
	"strings"
	"testing"
)

// TestReconcileUnsupportedPlatformWrapsErrUnsupported verifies that on
// platforms where lockState returns ErrUnsupported, Reconcile fails with
// ErrUnsupported wrapped in "admission reconcile:" context. It compiles
// only where the other-platform lock stub applies.
func TestReconcileUnsupportedPlatformWrapsErrUnsupported(t *testing.T) {
	g := &Gate{root: t.TempDir()}

	err := g.Reconcile()
	if err == nil {
		t.Fatal("Reconcile = nil, want ErrUnsupported")
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Reconcile error = %v, want ErrUnsupported", err)
	}
	if got := err.Error(); !strings.Contains(got, "admission reconcile:") {
		t.Fatalf("Reconcile error %q missing reconcile prefix", got)
	}
}

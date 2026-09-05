//go:build !darwin && !linux

package admission

import (
	"errors"
	"testing"
)

func TestLockReturnsErrUnsupported(t *testing.T) {
	_, err := lockState(t.TempDir())
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("lockState() = %v, want ErrUnsupported", err)
	}
}

func TestUnlockNilIsIdempotent(t *testing.T) {
	u, err := lockState(t.TempDir())
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("lockState() = %v, want ErrUnsupported", err)
	}
	// On unsupported platforms unlock is nil; calling it should not
	// panic. However, since we return nil, the caller must guard
	// against nil. Verify the contract.
	if u != nil {
		u() // would panic if called on a nil func
	}
}

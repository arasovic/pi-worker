package admission

import (
	"fmt"
	"testing"
)

// --- seam helpers ---

func withOwnerGetpid(t *testing.T, pid int) {
	t.Helper()
	orig := ownerGetpid
	ownerGetpid = func() int { return pid }
	t.Cleanup(func() { ownerGetpid = orig })
}

func withPidExists(t *testing.T, fn func(int) (bool, error)) {
	t.Helper()
	orig := pidExists
	pidExists = fn
	t.Cleanup(func() { pidExists = orig })
}

func withPidCreateTime(t *testing.T, fn func(int) (int64, error)) {
	t.Helper()
	orig := pidCreateTime
	pidCreateTime = fn
	t.Cleanup(func() { pidCreateTime = orig })
}

// --- currentOwner tests ---

func TestCurrentOwnerValid(t *testing.T) {
	withPidCreateTime(t, func(_ int) (int64, error) { return 1000, nil })
	owner, err := currentOwner()
	if err != nil {
		t.Fatalf("currentOwner() error: %v", err)
	}
	if owner.PID <= 0 {
		t.Errorf("PID = %d, want positive", owner.PID)
	}
	if owner.CreateTime != 1000 {
		t.Errorf("CreateTime = %d, want 1000", owner.CreateTime)
	}
}

func TestCurrentOwnerLookupError(t *testing.T) {
	withPidCreateTime(t, func(_ int) (int64, error) { return 0, fmt.Errorf("no process") })
	_, err := currentOwner()
	if err == nil {
		t.Fatal("currentOwner() = nil, want error")
	}
}

func TestCurrentOwnerZeroCreateTime(t *testing.T) {
	withPidCreateTime(t, func(_ int) (int64, error) { return 0, nil })
	_, err := currentOwner()
	if err == nil {
		t.Fatal("currentOwner() = nil, want error for zero createTime")
	}
}

func TestCurrentOwnerNegativeCreateTime(t *testing.T) {
	withPidCreateTime(t, func(_ int) (int64, error) { return -1, nil })
	_, err := currentOwner()
	if err == nil {
		t.Fatal("currentOwner() = nil, want error for negative createTime")
	}
}

// --- ownerStatus tests ---

func TestOwnerStatusSame(t *testing.T) {
	id := ownerIdentity{PID: 100, CreateTime: 1000}
	withPidExists(t, func(pid int) (bool, error) { return pid == 100, nil })
	withPidCreateTime(t, func(pid int) (int64, error) {
		if pid == 100 {
			return 1000, nil
		}
		return 0, fmt.Errorf("unknown pid %d", pid)
	})
	if got := ownerStatus(id); got != ownerSame {
		t.Errorf("ownerStatus(same) = %v, want ownerSame", got)
	}
}

func TestOwnerStatusAbsentPID(t *testing.T) {
	id := ownerIdentity{PID: 100, CreateTime: 1000}
	withPidExists(t, func(_ int) (bool, error) { return false, nil })
	withPidCreateTime(t, func(_ int) (int64, error) {
		return 0, fmt.Errorf("no such process")
	})
	if got := ownerStatus(id); got != ownerStale {
		t.Errorf("ownerStatus(absent) = %v, want ownerStale", got)
	}
}

func TestOwnerStatusPIDReuse(t *testing.T) {
	// PID exists but with a different creation time (PID reuse).
	id := ownerIdentity{PID: 100, CreateTime: 1000}
	withPidExists(t, func(_ int) (bool, error) { return true, nil })
	withPidCreateTime(t, func(_ int) (int64, error) { return 9999, nil })
	if got := ownerStatus(id); got != ownerStale {
		t.Errorf("ownerStatus(reused PID) = %v, want ownerStale", got)
	}
}

func TestOwnerStatusExistsError(t *testing.T) {
	id := ownerIdentity{PID: 100, CreateTime: 1000}
	withPidExists(t, func(_ int) (bool, error) { return false, fmt.Errorf("perm denied") })
	if got := ownerStatus(id); got != ownerUncertain {
		t.Errorf("ownerStatus(exists error) = %v, want ownerUncertain", got)
	}
}

func TestOwnerStatusCreateTimeError(t *testing.T) {
	id := ownerIdentity{PID: 100, CreateTime: 1000}
	withPidExists(t, func(_ int) (bool, error) { return true, nil })
	withPidCreateTime(t, func(_ int) (int64, error) { return 0, fmt.Errorf("err") })
	if got := ownerStatus(id); got != ownerUncertain {
		t.Errorf("ownerStatus(createTime error) = %v, want ownerUncertain", got)
	}
}

func TestOwnerStatusInvalidStoredIdentity(t *testing.T) {
	tests := []struct {
		name   string
		stored ownerIdentity
	}{
		{"zero PID", ownerIdentity{PID: 0, CreateTime: 1000}},
		{"negative PID", ownerIdentity{PID: -1, CreateTime: 1000}},
		{"zero CreateTime", ownerIdentity{PID: 100, CreateTime: 0}},
		{"negative CreateTime", ownerIdentity{PID: 100, CreateTime: -1}},
		{"both zero", ownerIdentity{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ownerStatus(tt.stored); got != ownerUncertain {
				t.Errorf("ownerStatus(%+v) = %v, want ownerUncertain", tt.stored, got)
			}
		})
	}
}

func TestOwnerStatusUncertainNeverStale(t *testing.T) {
	// An invalid stored identity must never be classified as stale,
	// even when pidExists returns true.
	id := ownerIdentity{PID: 100, CreateTime: 0}
	withPidExists(t, func(_ int) (bool, error) { return true, nil })
	withPidCreateTime(t, func(_ int) (int64, error) { return 1000, nil })
	if got := ownerStatus(id); got == ownerStale {
		t.Errorf("ownerStatus(invalid stored) = %v, must not be ownerStale", got)
	}
}

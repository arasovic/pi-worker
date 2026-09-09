//go:build darwin

package background

import (
	"os"
	"testing"
)

// workerHostPipeCapacityBytes reports 0 on darwin: the pipe capacity is
// not queryable there through x/sys. Tests fall back to a fixed payload
// far above the default pipe capacity. Linux implements the same
// function with F_GETPIPE_SZ.
func workerHostPipeCapacityBytes(t *testing.T, f *os.File) int {
	t.Helper()
	_ = f
	return 0
}

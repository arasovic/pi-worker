//go:build linux

package background

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// workerHostPipeCapacityBytes reports the kernel pipe capacity behind f
// in bytes via F_GETPIPE_SZ, the number of bytes a write can buffer
// before it blocks. Access goes through SyscallConn so the pipe's
// O_NONBLOCK mode — and with it the deadline support of the os.File
// wrapper — is never altered. Only Linux reports a capacity; other
// platforms implement the same function returning 0.
func workerHostPipeCapacityBytes(t *testing.T, f *os.File) int {
	t.Helper()
	conn, err := f.SyscallConn()
	if err != nil {
		t.Fatalf("syscall conn on pipe: %v", err)
	}
	var sz int
	err = conn.Control(func(fd uintptr) {
		sz, err = unix.FcntlInt(fd, unix.F_GETPIPE_SZ, 0)
	})
	if err != nil {
		t.Fatalf("fcntl F_GETPIPE_SZ: %v", err)
	}
	if sz <= 0 {
		t.Fatalf("fcntl F_GETPIPE_SZ returned %d, want a positive capacity", sz)
	}
	return sz
}

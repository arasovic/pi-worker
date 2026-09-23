//go:build darwin

package run

import "golang.org/x/sys/unix"

// defaultProcessEnviron reads one process's environment from the
// kern.procargs2 buffer. Apple system programs expose no environment
// this way; that is expected and not worked around.
func defaultProcessEnviron(pid int32) ([]string, error) {
	buf, err := unix.SysctlRaw("kern.procargs2", int(pid))
	if err != nil {
		return nil, err
	}
	return procargsEnvironment(buf), nil
}

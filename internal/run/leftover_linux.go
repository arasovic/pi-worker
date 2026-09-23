//go:build linux

package run

import "github.com/shirou/gopsutil/v4/process"

// defaultProcessEnviron reads one process's environment through gopsutil.
func defaultProcessEnviron(pid int32) ([]string, error) {
	p, err := process.NewProcess(pid)
	if err != nil {
		return nil, err
	}
	return p.Environ()
}

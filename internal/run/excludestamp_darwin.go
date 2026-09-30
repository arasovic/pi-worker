//go:build darwin

package run

import (
	"os"
	"syscall"
	"time"
)

func statusChangeTime(info os.FileInfo) time.Time {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}
	}
	return time.Unix(st.Ctimespec.Sec, st.Ctimespec.Nsec)
}

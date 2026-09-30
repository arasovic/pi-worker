//go:build !darwin && !linux

package run

import (
	"os"
	"time"
)

// These platforms expose no status-change time here, so the stamp falls back
// to size, modification time and mode.
func statusChangeTime(_ os.FileInfo) time.Time {
	return time.Time{}
}

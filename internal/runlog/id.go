package runlog

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

const runIDTimestampFormat = "20060102T150405Z" // 16 bytes

// ParseRunID parses a run ID produced by RunID into its constituent parts.
// The only accepted shape is YYYYMMDDTHHMMSSZ-<positive canonical decimal pid>,
// for example 20260830T041530Z-12345. It rejects signs (+/-), leading-zero
// PIDs, zero or negative PIDs, trailing non-pid text, and timestamps with
// invalid calendar values. Trailing text after the PID is also rejected.
func ParseRunID(runID string) (time.Time, error) {
	const prefixLen = len(runIDTimestampFormat) // 16

	// Minimum valid id: 16-byte prefix + '-' + one digit suffix = 18.
	if len(runID) < prefixLen+2 {
		return time.Time{}, errors.New("runId: too short")
	}

	tsPart := runID[:prefixLen]
	if runID[prefixLen] != '-' {
		return time.Time{}, fmt.Errorf("runId: expected hyphen at position %d, got %q", prefixLen, runID[prefixLen])
	}
	pidPart := runID[prefixLen+1:]

	// Parse the timestamp portion strictly.
	t, err := time.Parse(runIDTimestampFormat, tsPart)
	if err != nil {
		return time.Time{}, fmt.Errorf("runId: timestamp %q invalid: %w", tsPart, err)
	}

	// Parse and validate the PID suffix.
	pid, err := strconv.Atoi(pidPart)
	if err != nil {
		return time.Time{}, fmt.Errorf("runId: pid %q is not a valid integer: %v", pidPart, err)
	}
	if pid <= 0 {
		return time.Time{}, fmt.Errorf("runId: pid %d is not positive", pid)
	}
	if strconv.Itoa(pid) != pidPart {
		return time.Time{}, fmt.Errorf("runId: pid %q does not round-trip", pidPart)
	}

	return t, nil
}

// ValidateRunID validates that runID matches the identity contract: it must
// parse structurally (see ParseRunID) and its embedded timestamp must equal
// startedAt truncated to whole seconds in UTC — the same check StartWithID
// performs before writing the record.
func ValidateRunID(runID string, startedAt time.Time) error {
	parsed, err := ParseRunID(runID)
	if err != nil {
		return err
	}
	want := startedAt.UTC().Truncate(time.Second).Format(runIDTimestampFormat)
	got := parsed.Format(runIDTimestampFormat)
	if got != want {
		return fmt.Errorf("runId: prefix %q does not match startedAt %q", got, want)
	}
	return nil
}

// Package piversion classifies and probes the host Pi semantic version.
package piversion

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"
)

const (
	// VerifiedVersion is the only Pi version verified by pi-worker.
	VerifiedVersion = "0.85.0"
	// MaxOutputBytes bounds untrusted version stdout from the child process.
	MaxOutputBytes = 4096
	// WaitDelay bounds cleanup when a descendant retains the child's pipes.
	WaitDelay = 100 * time.Millisecond
)

type Status string

const (
	StatusVerified   Status = "verified"
	StatusUnverified Status = "unverified"
	StatusInvalid    Status = "invalid"
)

// Classification is the safe projection of one Pi version output.
type Classification struct {
	Status Status
	// Version is populated only for syntactically valid output.
	Version string
}

// Classify trims outer whitespace and accepts exactly one semantic version.
// A valid version other than VerifiedVersion is unverified rather than fatal.
func Classify(output string) Classification {
	version := strings.TrimSpace(output)
	if !ValidSemanticVersion(version) {
		return Classification{Status: StatusInvalid}
	}
	if version == VerifiedVersion {
		return Classification{Status: StatusVerified, Version: version}
	}
	return Classification{Status: StatusUnverified, Version: version}
}

// Probe runs executable --version with bounded stdout and discarded stderr.
// It returns only trimmed stdout after a successful, clean process exit.
func Probe(ctx context.Context, executable string) (string, error) {
	cmd := exec.CommandContext(ctx, executable, "--version")
	cmd.WaitDelay = WaitDelay
	output := &boundedBuffer{limit: MaxOutputBytes}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", err
	}
	if output.exceeded {
		return "", errors.New("version output exceeds limit")
	}
	return strings.TrimSpace(output.String()), nil
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	originalLength := len(data)
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		b.exceeded = true
		return originalLength, nil
	}
	if len(data) > remaining {
		b.exceeded = true
		data = data[:remaining]
	}
	_, _ = b.buffer.Write(data)
	return originalLength, nil
}

func (b *boundedBuffer) String() string { return b.buffer.String() }

// ValidSemanticVersion implements the SemVer 2.0.0 grammar by hand, with no
// leading v and no display text.
func ValidSemanticVersion(version string) bool {
	if version == "" {
		return false
	}
	coreAndBuild := version
	if plus := strings.IndexByte(coreAndBuild, '+'); plus >= 0 {
		if !validIdentifiers(coreAndBuild[plus+1:], true) {
			return false
		}
		coreAndBuild = coreAndBuild[:plus]
	}
	core := coreAndBuild
	if dash := strings.IndexByte(coreAndBuild, '-'); dash >= 0 {
		if !validIdentifiers(coreAndBuild[dash+1:], false) {
			return false
		}
		core = coreAndBuild[:dash]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if !validNumericIdentifier(part) {
			return false
		}
	}
	return true
}

func validIdentifiers(value string, allowLeadingZero bool) bool {
	if value == "" {
		return false
	}
	for _, identifier := range strings.Split(value, ".") {
		if identifier == "" {
			return false
		}
		allDigits := true
		for i := 0; i < len(identifier); i++ {
			c := identifier[i]
			if !isASCIIAlphanumeric(c) && c != '-' {
				return false
			}
			if c < '0' || c > '9' {
				allDigits = false
			}
		}
		if allDigits && !allowLeadingZero && len(identifier) > 1 && identifier[0] == '0' {
			return false
		}
	}
	return true
}

func validNumericIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return len(value) == 1 || value[0] != '0'
}

func isASCIIAlphanumeric(value byte) bool {
	return (value >= '0' && value <= '9') || (value >= 'A' && value <= 'Z') || (value >= 'a' && value <= 'z')
}

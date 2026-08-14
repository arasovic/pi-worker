package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"unicode/utf8"
)

// Verification is the outcome of one run-level check command executed in
// the workspace after a completed run. A passing verification carries
// only the argv and a zero exit code: the excerpt, its truncation flag,
// and the full log path exist only for failing commands.
type Verification struct {
	Argv      []string `json:"argv"`
	ExitCode  int      `json:"exitCode"`
	Output    string   `json:"output,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
	LogFile   string   `json:"logFile,omitempty"`
}

// verifyHeadBytes and verifyTailBytes bound the excerpt of a long
// failing capture: the first 2 KiB and the last 6 KiB, so a caller sees
// both the first failure and the final state of the output.
const (
	verifyHeadBytes = 2 * 1024
	verifyTailBytes = 6 * 1024
)

// Verifier runs one command in a workspace directory and reports the
// process outcome.
type Verifier interface {
	Verify(ctx context.Context, dir string, argv []string) (Verification, error)
}

// DefaultVerifier executes commands directly with os/exec: no shell and
// no assembled command strings.
type DefaultVerifier struct{}

// NewDefaultVerifier returns the os/exec-backed verifier.
func NewDefaultVerifier() Verifier {
	return &DefaultVerifier{}
}

// Verify runs argv in dir via exec.CommandContext with the parent
// context, capturing combined stdout and stderr in order, and reports
// the exit code. A start failure such as a missing command is returned
// as an error rather than a normal non-zero exit, and so is a context
// that expires or is cancelled while the command runs: the wrapped
// context error stays recoverable with errors.Is, so a caller can tell
// an interrupted run from a failing check. A failing capture longer
// than the excerpt budget is reduced to its first 2 KiB and last 6 KiB
// with the elided middle marked, and the full capture is written to a
// pi-worker-verify-*.log file in the system temp directory; pi-worker
// leaves that file to the OS temp policy. A log-write failure such as
// an unwritable temp directory leaves LogFile empty without failing the
// verification.
func (v *DefaultVerifier) Verify(ctx context.Context, dir string, argv []string) (Verification, error) {
	if len(argv) == 0 {
		return Verification{}, fmt.Errorf("verification command is empty")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	var capture bytes.Buffer
	cmd.Stdout = &capture
	cmd.Stderr = &capture
	err := cmd.Run()
	verification := Verification{Argv: argv}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return verification, fmt.Errorf("verification context: %w", ctxErr)
	}
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return verification, err
		}
		verification.ExitCode = exitErr.ExitCode()
	}
	if verification.ExitCode == 0 {
		return verification, nil
	}
	output := capture.String()
	if len(output) <= verifyHeadBytes+verifyTailBytes {
		verification.Output = output
		return verification, nil
	}
	verification.Output = verifyExcerpt(output)
	verification.Truncated = true
	if logFile, err := writeVerifyLog(output); err == nil {
		verification.LogFile = logFile
	}
	return verification, nil
}

// verifyExcerpt keeps the first 2 KiB and the last 6 KiB of a long
// capture, moving each cut to a rune boundary so no multi-byte
// character is split, and marks the elided middle with the dropped byte
// count. The byte budgets are upper bounds: the head and the tail may
// come out a few bytes short of their budgets, never longer.
func verifyExcerpt(output string) string {
	headEnd := verifyHeadBytes
	for headEnd > 0 && !utf8.RuneStart(output[headEnd]) {
		headEnd--
	}
	tailStart := len(output) - verifyTailBytes
	for tailStart < len(output) && !utf8.RuneStart(output[tailStart]) {
		tailStart++
	}
	head := output[:headEnd]
	tail := output[tailStart:]
	return head + fmt.Sprintf("\n[... %d bytes elided ...]\n", tailStart-headEnd) + tail
}

// writeVerifyLog writes the full capture to a new pi-worker-verify-*.log
// file in the system temp directory and returns its path.
func writeVerifyLog(output string) (string, error) {
	file, err := os.CreateTemp("", "pi-worker-verify-*.log")
	if err != nil {
		return "", err
	}
	defer file.Close()
	if _, err := file.WriteString(output); err != nil {
		return "", err
	}
	return file.Name(), nil
}

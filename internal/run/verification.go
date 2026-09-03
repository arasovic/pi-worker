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
	capture := newVerifyCapture()
	cmd.Stdout = capture
	cmd.Stderr = capture
	err := cmd.Run()
	verification := Verification{Argv: argv}
	if ctxErr := ctx.Err(); ctxErr != nil {
		capture.discardLog()
		return verification, fmt.Errorf("verification context: %w", ctxErr)
	}
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			capture.discardLog()
			return verification, err
		}
		verification.ExitCode = exitErr.ExitCode()
	}
	if verification.ExitCode == 0 {
		capture.discardLog()
		return verification, nil
	}
	if !capture.truncated {
		verification.Output = capture.short.String()
		return verification, nil
	}
	verification.Output = capture.excerpt()
	verification.Truncated = true
	if logFile := capture.finishLog(); logFile != "" {
		verification.LogFile = logFile
	}
	return verification, nil
}

// verifyCapture streams output to a temporary log after it exceeds the
// short-result budget while retaining only the bytes needed for the long
// result. The short buffer is bounded because it is discarded when the
// capture becomes long.
type verifyCapture struct {
	short     bytes.Buffer
	head      []byte
	tail      []byte
	total     int64
	truncated bool
	log       *os.File
	logPath   string
}

func newVerifyCapture() *verifyCapture {
	return &verifyCapture{}
}

func (c *verifyCapture) Write(p []byte) (int, error) {
	if !c.truncated && c.short.Len()+len(p) <= verifyHeadBytes+verifyTailBytes {
		c.total += int64(len(p))
		return c.short.Write(p)
	}
	if !c.truncated {
		c.truncated = true
		old := c.short.Bytes()
		headBytes := len(old)
		if headBytes > verifyHeadBytes+1 {
			headBytes = verifyHeadBytes + 1
		}
		c.head = append(c.head, old[:headBytes]...)
		if len(c.head) < verifyHeadBytes+1 {
			need := verifyHeadBytes + 1 - len(c.head)
			if need > len(p) {
				need = len(p)
			}
			c.head = append(c.head, p[:need]...)
		}
		c.appendTail(old)
		c.appendTail(p)
		c.total += int64(len(p))
		if err := c.startLog(); err == nil {
			c.writeLog(old)
			c.writeLog(p)
		}
		c.short = bytes.Buffer{}
		return len(p), nil
	}
	c.appendTail(p)
	c.total += int64(len(p))
	c.writeLog(p)
	return len(p), nil
}

func (c *verifyCapture) appendTail(p []byte) {
	if len(p) >= verifyTailBytes {
		c.tail = append(c.tail[:0], p[len(p)-verifyTailBytes:]...)
		return
	}
	if overflow := len(c.tail) + len(p) - verifyTailBytes; overflow > 0 {
		copy(c.tail, c.tail[overflow:])
		c.tail = c.tail[:len(c.tail)-overflow]
	}
	c.tail = append(c.tail, p...)
}

func (c *verifyCapture) startLog() error {
	file, err := os.CreateTemp("", "pi-worker-verify-*.log")
	if err != nil {
		return err
	}
	c.log = file
	c.logPath = file.Name()
	return nil
}

func (c *verifyCapture) writeLog(p []byte) {
	if c.log == nil {
		return
	}
	if _, err := c.log.Write(p); err != nil {
		_ = c.log.Close()
		c.log = nil
	}
}

func (c *verifyCapture) finishLog() string {
	if c.log == nil {
		c.removeLog()
		return ""
	}
	_ = c.log.Close()
	c.log = nil
	return c.logPath
}

func (c *verifyCapture) discardLog() {
	if c.log != nil {
		_ = c.log.Close()
		c.log = nil
	}
	c.removeLog()
}

func (c *verifyCapture) removeLog() {
	if c.logPath != "" {
		_ = os.Remove(c.logPath)
		c.logPath = ""
	}
}

func (c *verifyCapture) excerpt() string {
	headEnd := verifyHeadBytes
	for headEnd > 0 && !utf8.RuneStart(c.head[headEnd]) {
		headEnd--
	}
	tailStart := 0
	for tailStart < len(c.tail) && !utf8.RuneStart(c.tail[tailStart]) {
		tailStart++
	}
	return string(c.head[:headEnd]) +
		fmt.Sprintf("\n[... %d bytes elided ...]\n", c.total-int64(verifyTailBytes)+int64(tailStart)-int64(headEnd)) +
		string(c.tail[tailStart:])
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

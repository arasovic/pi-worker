package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/arasovic/pi-worker/internal/contracts"
	"github.com/arasovic/pi-worker/internal/pi"
)

// scriptedVerifier is a concurrency-safe fake Verifier for controller
// tests. It records every invocation with its context, directory, and
// argv, and returns the configured result and error.
type scriptedVerifier struct {
	mu    sync.Mutex
	ctxs  []context.Context
	dirs  []string
	argvs [][]string

	result Verification
	err    error
}

func (v *scriptedVerifier) Verify(ctx context.Context, dir string, argv []string) (Verification, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.ctxs = append(v.ctxs, ctx)
	v.dirs = append(v.dirs, dir)
	v.argvs = append(v.argvs, append([]string(nil), argv...))
	return v.result, v.err
}

func (v *scriptedVerifier) callCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.ctxs)
}

func (v *scriptedVerifier) dirsSeen() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.dirs...)
}

func (v *scriptedVerifier) argvsSeen() [][]string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([][]string(nil), v.argvs...)
}

// verifyHelperArgs returns the argv that re-executes this test binary as
// a scripted verification command, and switches it into command mode via
// the PI_WORKER_VERIFY_HELPER environment variable. exit and lines
// control the command's exit code and numbered output length.
func verifyHelperArgs(t *testing.T, exit, lines string) []string {
	t.Helper()
	t.Setenv("PI_WORKER_VERIFY_HELPER", "1")
	t.Setenv("PI_WORKER_VERIFY_EXIT", exit)
	t.Setenv("PI_WORKER_VERIFY_LINES", lines)
	return []string{os.Args[0], "-test.run=TestVerifyHelperProcess"}
}

// TestVerifyHelperProcess is the re-exec target for default verifier
// tests: with PI_WORKER_VERIFY_HELPER set it prints its working
// directory and a numbered output, then exits with PI_WORKER_VERIFY_EXIT
// (3 by default). Without the marker it does nothing, so the test passes
// when the suite runs it directly.
func TestVerifyHelperProcess(t *testing.T) {
	if os.Getenv("PI_WORKER_VERIFY_HELPER") != "1" {
		return
	}
	if cwd, err := os.Getwd(); err == nil {
		fmt.Fprintf(os.Stderr, "cwd=%s\n", cwd)
	}
	if raw := os.Getenv("PI_WORKER_VERIFY_SLEEP_MS"); raw != "" {
		if ms, err := strconv.Atoi(raw); err == nil && ms > 0 {
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
	}
	lines := 0
	if raw := os.Getenv("PI_WORKER_VERIFY_LINES"); raw != "" {
		lines, _ = strconv.Atoi(raw)
	}
	for i := 1; i <= lines; i++ {
		fmt.Fprintf(os.Stdout, "line-%04d\n", i)
	}
	exit := 3
	if raw := os.Getenv("PI_WORKER_VERIFY_EXIT"); raw != "" {
		if code, err := strconv.Atoi(raw); err == nil {
			exit = code
		}
	}
	os.Exit(exit)
}

func TestDefaultVerifierPassingRunCarriesNoOutput(t *testing.T) {
	verification, err := NewDefaultVerifier().Verify(context.Background(), t.TempDir(), verifyHelperArgs(t, "0", "0"))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if verification.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", verification.ExitCode)
	}
	if verification.Output != "" || verification.Truncated || verification.LogFile != "" {
		t.Fatalf("passing verification carried output fields: %#v", verification)
	}
	data, err := json.Marshal(verification)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{`"output"`, `"truncated"`, `"logFile"`} {
		if strings.Contains(string(data), field) {
			t.Fatalf("passing verification JSON contains %s: %s", field, data)
		}
	}
	if !strings.Contains(string(data), `"argv"`) || !strings.Contains(string(data), `"exitCode":0`) {
		t.Fatalf("passing verification JSON lost argv or exitCode: %s", data)
	}
}

func TestDefaultVerifierFailingRunCarriesExitCodeAndExcerpt(t *testing.T) {
	verification, err := NewDefaultVerifier().Verify(context.Background(), t.TempDir(), verifyHelperArgs(t, "3", "0"))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if verification.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", verification.ExitCode)
	}
	if !strings.Contains(verification.Output, "cwd=") {
		t.Fatalf("excerpt missing the captured output: %q", verification.Output)
	}
	if verification.Truncated {
		t.Fatalf("short capture marked truncated: %#v", verification)
	}
	if verification.LogFile != "" {
		t.Fatalf("short capture wrote a log file: %#v", verification)
	}
}

func TestDefaultVerifierTruncatesLongCaptureToHeadAndTail(t *testing.T) {
	workspace := t.TempDir()
	verification, err := NewDefaultVerifier().Verify(context.Background(), workspace, verifyHelperArgs(t, "3", "1500"))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	var full strings.Builder
	fmt.Fprintf(&full, "cwd=%s\n", workspace)
	for i := 1; i <= 1500; i++ {
		fmt.Fprintf(&full, "line-%04d\n", i)
	}
	capture := full.String()
	if len(capture) <= verifyHeadBytes+verifyTailBytes {
		t.Fatalf("test capture is not longer than the excerpt budget: %d bytes", len(capture))
	}
	if verification.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", verification.ExitCode)
	}
	if !verification.Truncated {
		t.Fatalf("long capture not marked truncated: %#v", verification)
	}
	if !strings.HasPrefix(verification.Output, capture[:verifyHeadBytes]) {
		t.Fatalf("excerpt lost the head:\n%s", verification.Output)
	}
	if !strings.HasSuffix(verification.Output, capture[len(capture)-verifyTailBytes:]) {
		t.Fatalf("excerpt lost the tail:\n%s", verification.Output)
	}
	if !strings.Contains(verification.Output, "elided") {
		t.Fatalf("excerpt has no elision marker:\n%s", verification.Output)
	}
	if len(verification.Output) >= len(capture) {
		t.Fatalf("excerpt is not shorter than the full capture")
	}
	if verification.LogFile == "" {
		t.Fatalf("truncated capture wrote no log file")
	}
	if base := filepath.Base(verification.LogFile); !strings.HasPrefix(base, "pi-worker-verify-") || !strings.HasSuffix(base, ".log") {
		t.Fatalf("log file name %q does not match pi-worker-verify-*.log", base)
	}
	logged, err := os.ReadFile(verification.LogFile)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if string(logged) != capture {
		t.Fatalf("log file does not contain the full capture")
	}
}

func TestVerifyExcerptKeepsUTF8CharactersWhole(t *testing.T) {
	check := "\u2713"
	head := strings.Repeat("a", verifyHeadBytes-2) + check
	tail := check + strings.Repeat("z", verifyTailBytes-2)
	output := head + strings.Repeat("x", 7) + tail
	if len(output) <= verifyHeadBytes+verifyTailBytes {
		t.Fatalf("test output is not longer than the excerpt budget: %d bytes", len(output))
	}
	excerpt := verifyExcerpt(output)
	if !utf8.ValidString(excerpt) {
		t.Fatalf("excerpt is not valid UTF-8: %q", excerpt)
	}
	want := strings.Repeat("a", verifyHeadBytes-2) +
		"\n[... 13 bytes elided ...]\n" +
		strings.Repeat("z", verifyTailBytes-2)
	if excerpt != want {
		t.Fatalf("excerpt = %q, want %q", excerpt, want)
	}
}

func TestDefaultVerifierKeepsResultWhenLogWriteFails(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "unwritable"))
	verification, err := NewDefaultVerifier().Verify(context.Background(), workspace, verifyHelperArgs(t, "3", "1500"))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if verification.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", verification.ExitCode)
	}
	if !verification.Truncated {
		t.Fatalf("long capture not marked truncated: %#v", verification)
	}
	var full strings.Builder
	fmt.Fprintf(&full, "cwd=%s\n", workspace)
	for i := 1; i <= 1500; i++ {
		fmt.Fprintf(&full, "line-%04d\n", i)
	}
	capture := full.String()
	if !strings.HasPrefix(verification.Output, capture[:verifyHeadBytes]) {
		t.Fatalf("excerpt lost the head:\n%s", verification.Output)
	}
	if !strings.HasSuffix(verification.Output, capture[len(capture)-verifyTailBytes:]) {
		t.Fatalf("excerpt lost the tail:\n%s", verification.Output)
	}
	if !strings.Contains(verification.Output, "elided") {
		t.Fatalf("excerpt has no elision marker:\n%s", verification.Output)
	}
	if verification.LogFile != "" {
		t.Fatalf("log-write failure left a log path: %q", verification.LogFile)
	}
}

func TestDefaultVerifierRunsInTheWorkspaceDirectory(t *testing.T) {
	workspace := t.TempDir()
	processCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	verification, err := NewDefaultVerifier().Verify(context.Background(), workspace, verifyHelperArgs(t, "3", "0"))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !strings.Contains(verification.Output, "cwd="+workspace) {
		t.Fatalf("command did not run in the workspace: %q", verification.Output)
	}
	if strings.Contains(verification.Output, "cwd="+processCwd) {
		t.Fatalf("command ran in the process working directory: %q", verification.Output)
	}
}

func TestDefaultVerifierReportsStartFailureAsError(t *testing.T) {
	verification, err := NewDefaultVerifier().Verify(context.Background(), t.TempDir(), []string{"pi-worker-no-such-command"})
	if err == nil {
		t.Fatalf("Verify accepted a missing command")
	}
	if verification.ExitCode != 0 {
		t.Fatalf("start failure reported exit code %d, want 0", verification.ExitCode)
	}
}

func TestDefaultVerifierReportsContextExpiryAsError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	t.Setenv("PI_WORKER_VERIFY_SLEEP_MS", "5000")
	verification, err := NewDefaultVerifier().Verify(ctx, t.TempDir(), verifyHelperArgs(t, "0", "0"))
	if err == nil {
		t.Fatalf("Verify accepted a command that outlived the context")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expiry error = %v, want errors.Is(err, context.DeadlineExceeded)", err)
	}
	if verification.ExitCode != 0 {
		t.Fatalf("expired command reported exit code %d, want 0", verification.ExitCode)
	}
}

func TestControllerRunsVerificationOnceWithWorkspaceAndArgv(t *testing.T) {
	worker := newScriptedWorker()
	verifier := &scriptedVerifier{result: Verification{Argv: []string{"go", "test", "./..."}, ExitCode: 0}}
	ctx := context.Background()
	req := validRequest("a", "b", "c")
	req.Verify = []string{"go", "test", "./..."}
	result, err := New(worker, WithVerifier(verifier)).Run(ctx, req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != contracts.RunCompleted {
		t.Fatalf("status = %q, want completed", result.Status)
	}
	if result.Verification == nil {
		t.Fatalf("completed run carried no verification")
	}
	if result.Verification.ExitCode != 0 {
		t.Fatalf("verification exit code = %d, want 0", result.Verification.ExitCode)
	}
	if result.Verification.Output != "" {
		t.Fatalf("passing verification carried output: %#v", result.Verification)
	}
	if n := verifier.callCount(); n != 1 {
		t.Fatalf("verification ran %d times, want once for the whole run", n)
	}
	if dirs := verifier.dirsSeen(); len(dirs) != 1 || dirs[0] != req.Workspace {
		t.Fatalf("verification dirs = %v, want [%q]", dirs, req.Workspace)
	}
	if argvs := verifier.argvsSeen(); len(argvs) != 1 || !equalStrings(argvs[0], req.Verify) {
		t.Fatalf("verification argvs = %v, want [%v]", argvs, req.Verify)
	}
}

func TestControllerSkipsVerificationWithoutCompletedRun(t *testing.T) {
	tests := []struct {
		name    string
		results map[string]pi.WorkerResult
		want    contracts.RunStatus
	}{
		{
			name: "partial",
			results: map[string]pi.WorkerResult{
				"a": {Status: pi.StatusCompleted, Explanation: "ok"},
				"b": {Status: pi.StatusFailed, Error: "boom"},
			},
			want: contracts.RunPartial,
		},
		{
			name: "failed",
			results: map[string]pi.WorkerResult{
				"a": {Status: pi.StatusFailed, Error: "boom"},
				"b": {Status: pi.StatusFailed, Error: "boom"},
			},
			want: contracts.RunFailed,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			worker := newScriptedWorker()
			worker.results = test.results
			verifier := &scriptedVerifier{result: Verification{ExitCode: 0}}
			tasks := make([]string, 0, len(test.results))
			for task := range test.results {
				tasks = append(tasks, task)
			}
			req := validRequest(tasks...)
			req.Verify = []string{"go", "test", "./..."}
			result, err := New(worker, WithVerifier(verifier)).Run(context.Background(), req)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if result.Status != test.want {
				t.Fatalf("status = %q, want %q", result.Status, test.want)
			}
			if result.Verification != nil {
				t.Fatalf("%s run carried a verification: %#v", test.want, result.Verification)
			}
			if verifier.callCount() != 0 {
				t.Fatalf("verification ran %d times on a %s run", verifier.callCount(), test.want)
			}
		})
	}
}

func TestControllerSkipsVerificationOnDoneContext(t *testing.T) {
	tests := []struct {
		name string
		ctx  func() context.Context
		want contracts.RunStatus
	}{
		{
			name: "cancelled",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			want: contracts.RunCancelled,
		},
		{
			name: "timed out",
			ctx: func() context.Context {
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer cancel()
				return ctx
			},
			want: contracts.RunTimedOut,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Workers settle completed before observing the parent state,
			// so the aggregate still reports cancelled/timed-out and the
			// command must not run: a done parent would fail it for an
			// unrelated reason.
			worker := newScriptedWorker()
			worker.ignoreCtx = true
			verifier := &scriptedVerifier{result: Verification{ExitCode: 0}}
			req := validRequest("a", "b")
			req.Verify = []string{"go", "test", "./..."}
			result, err := New(worker, WithVerifier(verifier)).Run(test.ctx(), req)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if result.Status != test.want {
				t.Fatalf("status = %q, want %q", result.Status, test.want)
			}
			if result.Verification != nil {
				t.Fatalf("%s run carried a verification: %#v", test.want, result.Verification)
			}
			if verifier.callCount() != 0 {
				t.Fatalf("verification ran %d times on a %s run", verifier.callCount(), test.want)
			}
		})
	}
}

func TestControllerPropagatesVerificationContextExpiry(t *testing.T) {
	worker := newScriptedWorker()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := validRequest("a", "b")
	req.Workspace = t.TempDir()
	req.Verify = verifyHelperArgs(t, "0", "0")
	t.Setenv("PI_WORKER_VERIFY_SLEEP_MS", "5000")
	_, err := New(worker, WithVerifier(NewDefaultVerifier())).Run(ctx, req)
	if err == nil {
		t.Fatalf("Run accepted a verification that outlived the context")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expiry error = %v, want errors.Is(err, context.DeadlineExceeded)", err)
	}
}

func TestControllerWithoutVerifierBehavesAsBefore(t *testing.T) {
	worker := newScriptedWorker()
	req := validRequest("a", "b")
	req.Verify = []string{"go", "test", "./..."}
	result, err := New(worker).Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != contracts.RunCompleted {
		t.Fatalf("status = %q, want completed", result.Status)
	}
	if result.Verification != nil {
		t.Fatalf("verifier-less controller carried a verification: %#v", result.Verification)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), `"verification"`) {
		t.Fatalf("verifier-less result JSON carries a verification: %s", data)
	}
}

func TestControllerPropagatesVerifierStartFailure(t *testing.T) {
	worker := newScriptedWorker()
	verifier := &scriptedVerifier{err: fmt.Errorf("exec: no such command")}
	req := validRequest("a", "b")
	req.Verify = []string{"no-such-command"}
	result, err := New(worker, WithVerifier(verifier)).Run(context.Background(), req)
	if err == nil {
		t.Fatalf("Run accepted a verifier start failure")
	}
	if result.Status != contracts.RunCompleted {
		t.Fatalf("status = %q, want completed", result.Status)
	}
	if result.Verification != nil {
		t.Fatalf("start failure carried a verification: %#v", result.Verification)
	}
}

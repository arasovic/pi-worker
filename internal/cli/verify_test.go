package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/pi"
)

// cliVerifyHelperArgs returns the argv that re-executes this test binary
// as a scripted verification command, switching it into helper mode via
// PI_WORKER_CLI_VERIFY_HELPER. exit and lines control the command's exit
// code and numbered output length, so CLI tests exercise the real
// DefaultVerifier without launching a shell or Pi.
func cliVerifyHelperArgs(t *testing.T, exit, lines string) []string {
	t.Helper()
	t.Setenv("PI_WORKER_CLI_VERIFY_HELPER", "1")
	t.Setenv("PI_WORKER_CLI_VERIFY_EXIT", exit)
	t.Setenv("PI_WORKER_CLI_VERIFY_LINES", lines)
	return []string{os.Args[0], "-test.run=TestCLIVerifyHelperProcess"}
}

// TestCLIVerifyHelperProcess is the re-exec target for CLI verification
// tests: with the marker set it sleeps when asked, prints numbered
// output, and exits with the requested code (3 by default). Without the
// marker it does nothing, so the test passes when the suite runs it
// directly.
func TestCLIVerifyHelperProcess(t *testing.T) {
	if os.Getenv("PI_WORKER_CLI_VERIFY_HELPER") != "1" {
		return
	}
	if raw := os.Getenv("PI_WORKER_CLI_VERIFY_SLEEP_MS"); raw != "" {
		if ms, err := strconv.Atoi(raw); err == nil && ms > 0 {
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
	}
	lines := 0
	if raw := os.Getenv("PI_WORKER_CLI_VERIFY_LINES"); raw != "" {
		lines, _ = strconv.Atoi(raw)
	}
	for i := 1; i <= lines; i++ {
		fmt.Fprintf(os.Stdout, "check-line-%04d\n", i)
	}
	exit := 3
	if raw := os.Getenv("PI_WORKER_CLI_VERIFY_EXIT"); raw != "" {
		if code, err := strconv.Atoi(raw); err == nil {
			exit = code
		}
	}
	os.Exit(exit)
}

func TestRunVerifyRejectsShellCharactersAndEmptyValues(t *testing.T) {
	installConfigPath(t, filepath.Join(t.TempDir(), "config.json"))
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "pipe", value: "a | b", want: "'|'"},
		{name: "ampersand", value: "go build ./... && go test ./...", want: "'&'"},
		{name: "semicolon", value: "a; b", want: "';'"},
		{name: "less than", value: "a < b", want: "'<'"},
		{name: "greater than", value: "a > b", want: "'>'"},
		{name: "dollar", value: "echo $HOME", want: "'$'"},
		{name: "backtick", value: "echo `date`", want: "'`'"},
		{name: "newline", value: "a\nb", want: "'\\n'"},
		{name: "single quote", value: "go test -run 'TestFoo Bar'", want: "'\\''"},
		{name: "double quote", value: "echo \"hi there\"", want: "'\"'"},
		{name: "backslash", value: "go test -run Foo\\ Bar", want: "'\\\\'"},
		{name: "empty", value: "", want: "empty or whitespace-only"},
		{name: "whitespace only", value: "   ", want: "empty or whitespace-only"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
			code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--verify", test.value, "--task", "x"}, "")
			if code != 2 {
				t.Fatalf("exit = %d, want 2; stderr = %q", code, stderr)
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, test.want) {
				t.Fatalf("stderr missing %q: %q", test.want, stderr)
			}
			if !strings.Contains(stderr, "usage:") {
				t.Fatalf("stderr missing usage text: %q", stderr)
			}
			if fake.callCount() != 0 {
				t.Fatalf("worker invoked %d times, want 0", fake.callCount())
			}
		})
	}
}

func TestRunVerifyFlagGivenTwiceIsUsageError(t *testing.T) {
	installConfigPath(t, filepath.Join(t.TempDir(), "config.json"))
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--verify", "go test ./...", "--verify", "go vet ./...", "--task", "x"}, "")
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %q", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "flag --verify specified more than once") {
		t.Fatalf("stderr = %q", stderr)
	}
	if fake.callCount() != 0 {
		t.Fatalf("worker invoked %d times, want 0", fake.callCount())
	}
}

func TestRunVerifyPassingExitsZeroAndCarriesArgvOnly(t *testing.T) {
	fake := installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "All done."})
	verify := strings.Join(cliVerifyHelperArgs(t, "0", "0"), " ")
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--json", "--verify", verify}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	document := decodeJSONObject(t, stdout)
	assertExactJSONKeys(t, document, "changes", "schemaVersion", "status", "workers", "verification")
	if document["status"] != "completed" {
		t.Fatalf("status = %v, want completed", document["status"])
	}
	verification, ok := document["verification"].(map[string]any)
	if !ok {
		t.Fatalf("verification = %#v, want object", document["verification"])
	}
	assertExactJSONKeys(t, verification, "argv", "exitCode")
	if verification["exitCode"] != float64(0) {
		t.Fatalf("verification exitCode = %v, want 0", verification["exitCode"])
	}
	if fake.callCount() != 1 {
		t.Fatalf("worker calls = %d, want 1", fake.callCount())
	}
}

func TestRunVerifyPassingHumanPrintsOneShortLine(t *testing.T) {
	installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "All done."})
	verify := strings.Join(cliVerifyHelperArgs(t, "0", "0"), " ")
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--verify", verify}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	requireChangesTail(t, stdout, "worker 1: All done.\nverification: ok\n")
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestRunVerifyFailingExitsSixAndKeepsStatusCompleted(t *testing.T) {
	fake := installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "All done."})
	verify := strings.Join(cliVerifyHelperArgs(t, "3", "2"), " ")
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--json", "--verify", verify}, "")
	if code != 6 {
		t.Fatalf("exit = %d, want 6; stderr = %q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty in json mode", stderr)
	}
	document := decodeJSONObject(t, stdout)
	assertExactJSONKeys(t, document, "changes", "schemaVersion", "status", "workers", "verification")
	if document["status"] != "completed" {
		t.Fatalf("status = %v, want completed (worker outcomes only)", document["status"])
	}
	verification, ok := document["verification"].(map[string]any)
	if !ok {
		t.Fatalf("verification = %#v, want object", document["verification"])
	}
	assertExactJSONKeys(t, verification, "argv", "exitCode", "output")
	if verification["exitCode"] != float64(3) {
		t.Fatalf("verification exitCode = %v, want 3", verification["exitCode"])
	}
	if !strings.Contains(verification["output"].(string), "check-line-0001") {
		t.Fatalf("verification output missing excerpt: %v", verification["output"])
	}
	if fake.callCount() != 1 {
		t.Fatalf("worker calls = %d, want 1", fake.callCount())
	}
}

func TestRunVerifyFailingHumanPrintsExitCodeAndExcerpt(t *testing.T) {
	installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "All done."})
	verify := strings.Join(cliVerifyHelperArgs(t, "3", "2"), " ")
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--verify", verify}, "")
	if code != 6 {
		t.Fatalf("exit = %d, want 6; stderr = %q", code, stderr)
	}
	requireChangesTail(t, stdout, "worker 1: All done.\n")
	if !strings.Contains(stderr, "pi-worker: verification failed with exit code 3") {
		t.Fatalf("stderr missing exit code: %q", stderr)
	}
	if !strings.Contains(stderr, "check-line-0001") || !strings.Contains(stderr, "check-line-0002") {
		t.Fatalf("stderr missing excerpt: %q", stderr)
	}
}

func TestRunVerifyFailingHumanPrintsLogPathForTruncatedCapture(t *testing.T) {
	installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "All done."})
	// 1300 numbered lines exceed the excerpt budget, forcing a truncated
	// excerpt backed by a full pi-worker-verify-*.log temp file.
	verify := strings.Join(cliVerifyHelperArgs(t, "3", "1300"), " ")
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--verify", verify}, "")
	if code != 6 {
		t.Fatalf("exit = %d, want 6; stderr = %q", code, stderr)
	}
	requireChangesTail(t, stdout, "worker 1: All done.\n")
	if !strings.Contains(stderr, "pi-worker: verification failed with exit code 3") {
		t.Fatalf("stderr missing exit code: %q", stderr)
	}
	if !strings.Contains(stderr, "[... ") || !strings.Contains(stderr, "bytes elided ...]") {
		t.Fatalf("stderr missing elision marker: %q", stderr)
	}
	if !strings.Contains(stderr, "pi-worker: verification log: ") {
		t.Fatalf("stderr missing log path: %q", stderr)
	}
	logPath := ""
	for _, line := range strings.Split(stderr, "\n") {
		if prefix := "pi-worker: verification log: "; strings.HasPrefix(line, prefix) {
			logPath = strings.TrimPrefix(line, prefix)
		}
	}
	if logPath == "" {
		t.Fatalf("no log path parsed from stderr: %q", stderr)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read verification log: %v", err)
	}
	if strings.Count(string(data), "check-line-") != 1300 {
		t.Fatalf("log has %d check lines, want 1300", strings.Count(string(data), "check-line-"))
	}
	t.Cleanup(func() { os.Remove(logPath) })
}

func TestRunWithoutVerifyKeepsJSONFreeOfVerification(t *testing.T) {
	installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "done"})
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	document := decodeJSONObject(t, stdout)
	assertExactJSONKeys(t, document, "changes", "schemaVersion", "status", "workers")
}

func TestRunVerifyContextExpiryExitsSevenNotSix(t *testing.T) {
	installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "ok"})
	verify := strings.Join(cliVerifyHelperArgs(t, "0", "0"), " ")
	t.Setenv("PI_WORKER_CLI_VERIFY_SLEEP_MS", "5000")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	code, stdout, stderr := runCLIWithContext(t, ctx, []string{"run", "--model", "acme/m-1", "--task", "go", "--verify", verify}, "")
	if code != 7 {
		t.Fatalf("exit = %d, want timed-out 7 (not verification 6); stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "verification") {
		t.Fatalf("stderr missing verification error: %q", stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty for the aborted run", stdout)
	}
}

func TestRunVerifyCarriesArgvSplitOnWhitespace(t *testing.T) {
	installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "ok"})
	verify := os.Args[0] + " -test.run=TestCLIVerifyHelperProcess"
	t.Setenv("PI_WORKER_CLI_VERIFY_HELPER", "1")
	t.Setenv("PI_WORKER_CLI_VERIFY_EXIT", "0")
	t.Setenv("PI_WORKER_CLI_VERIFY_LINES", "0")
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--json", "--verify", verify}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	var document struct {
		Verification *struct {
			Argv []string `json:"argv"`
		} `json:"verification"`
	}
	if err := json.Unmarshal([]byte(stdout), &document); err != nil {
		t.Fatalf("decode json stdout: %v (%q)", err, stdout)
	}
	if document.Verification == nil {
		t.Fatalf("no verification object: %q", stdout)
	}
	if len(document.Verification.Argv) != 2 || document.Verification.Argv[0] != os.Args[0] || document.Verification.Argv[1] != "-test.run=TestCLIVerifyHelperProcess" {
		t.Fatalf("argv = %v, want [%s -test.run=TestCLIVerifyHelperProcess]", document.Verification.Argv, os.Args[0])
	}
}

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/buildinfo"
	"github.com/arasovic/pi-worker/internal/contracts"
	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/run"
	"github.com/arasovic/pi-worker/internal/skillinstall"
	"github.com/arasovic/pi-worker/internal/testutil/fakepi/script"
)

// fakePiBin is the path of the fakepi helper binary built once per test run.
var fakePiBin string

const interruptHelperEnv = "PI_WORKER_INTERRUPT_HELPER"

func TestMain(m *testing.M) {
	// The signal-helper subprocess exits while blocked on stdin and never
	// reaches a worker. Skip the unrelated fake-Pi build so it can run with
	// a deliberately minimal environment.
	if os.Getenv(interruptHelperEnv) == "1" {
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "pi-worker-cli-fakepi-bin-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create fakepi build directory: %v\n", err)
		os.Exit(1)
	}
	fakePiBin = filepath.Join(dir, "fakepi")
	build := exec.Command("go", "build", "-o", fakePiBin, "github.com/arasovic/pi-worker/internal/testutil/fakepi")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build fakepi: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	originalRunVersionProbe := runVersionProbe
	runVersionProbe = func(context.Context) (string, error) { return "0.84.2", nil }
	code := m.Run()
	runVersionProbe = originalRunVersionProbe
	os.RemoveAll(dir)
	os.Exit(code)
}

// fakeWorker is the scripted worker installed through the newWorker seam.
type fakeWorker struct {
	defaultResult   pi.WorkerResult
	resultsByWorker map[int]pi.WorkerResult
	resultsByPrompt map[string]pi.WorkerResult
	requests        map[int]pi.WorkerRequest
	deadlines       map[int]time.Time
	hasDeadline     map[int]bool
	calls           int
	active          int
	maxActive       int
	startGate       chan struct{}
	startGateAt     int
	startGateClosed bool
	releaseByWorker map[int]chan struct{}
	releaseByPrompt map[string]chan struct{}
	completed       chan int
	ignoreContext   bool
	runHook         func()
	mu              sync.Mutex
}

func (f *fakeWorker) Run(ctx context.Context, req pi.WorkerRequest) (result pi.WorkerResult) {
	var scope *pi.WorkerScope

	f.mu.Lock()
	f.calls++
	f.requests[req.WorkerID] = req
	if deadline, ok := ctx.Deadline(); ok {
		f.deadlines[req.WorkerID] = deadline
		f.hasDeadline[req.WorkerID] = true
	}
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	if f.startGate != nil && !f.startGateClosed && f.active >= f.startGateAt {
		close(f.startGate)
		f.startGateClosed = true
	}
	f.mu.Unlock()

	if f.runHook != nil {
		f.runHook()
	}

	if req.Debug != nil {
		scope = req.Debug.Worker(req.WorkerID)
		scope.Log("phase=starting", "provider=acme", "model=m-1")
	}

	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()

	if scope != nil {
		defer func() {
			scope.Log("status="+result.Status, "total="+scope.Elapsed().String())
		}()
	}

	if f.startGate != nil {
		<-f.startGate
	}
	if ch := f.releaseByPrompt[req.Prompt]; ch != nil {
		select {
		case <-ch:
		case <-ctx.Done():
		}
	}
	if ch := f.releaseByWorker[req.WorkerID]; ch != nil {
		select {
		case <-ch:
		case <-ctx.Done():
		}
	}

	if err := ctx.Err(); err != nil && !f.ignoreContext {
		if errors.Is(err, context.DeadlineExceeded) {
			result = pi.WorkerResult{Model: req.Model, Status: pi.StatusTimedOut, Error: "timed out"}
		} else {
			result = pi.WorkerResult{Model: req.Model, Status: pi.StatusCancelled, Error: "cancelled"}
		}
	} else {
		f.mu.Lock()
		if byWorker, ok := f.resultsByWorker[req.WorkerID]; ok {
			result = byWorker
		} else if byPrompt, ok := f.resultsByPrompt[req.Prompt]; ok {
			result = byPrompt
		} else {
			result = f.defaultResult
		}
		f.mu.Unlock()
		if result.Model == "" {
			result.Model = req.Model
		}
	}

	if f.completed != nil {
		f.completed <- req.WorkerID
	}

	return result
}

func (f *fakeWorker) requestForWorker(id int) (pi.WorkerRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	request, ok := f.requests[id]
	return request, ok
}

func (f *fakeWorker) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeWorker) maxConcurrency() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxActive
}

func (f *fakeWorker) deadlineForWorker(id int) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	deadline, ok := f.deadlines[id]
	return deadline, ok && f.hasDeadline[id]
}

// installFakeWorker replaces the private newWorker seam so tests never
// launch the user's real Pi profile.
func installFakeWorker(t *testing.T, result pi.WorkerResult) *fakeWorker {
	t.Helper()
	fake := &fakeWorker{
		defaultResult:   result,
		resultsByWorker: make(map[int]pi.WorkerResult),
		resultsByPrompt: make(map[string]pi.WorkerResult),
		requests:        make(map[int]pi.WorkerRequest),
		deadlines:       make(map[int]time.Time),
		hasDeadline:     make(map[int]bool),
	}
	original := newWorker
	newWorker = func() pi.Worker { return fake }
	t.Cleanup(func() { newWorker = original })
	return fake
}

func runCLI(t *testing.T, args []string, stdin string) (int, string, string) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Main(args, strings.NewReader(stdin), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// runCLIWithContext drives the private Main seam with an explicit parent
// context so cancellation tests are deterministic and never send a real
// signal to the test process.
func runCLIWithContext(t *testing.T, ctx context.Context, args []string, stdin string) (int, string, string) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := mainWithContext(ctx, args, strings.NewReader(stdin), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// requireChangesTail asserts that human run output carries the
// worker-summary lines `want` verbatim and then exactly one
// change-manifest line and the final outcome line. The manifest line
// itself depends on the workspace's tree state at test time —
// "changes: 0 files, +0/-0" on a clean checkout, a dirty tree carrying
// measured counts and the dirty-before clause, "changes: omitted:
// <reason>" only when measurement could not run — so only its presence
// and position after the summaries are pinned here; the manifest's own
// tests pin its content.
func requireChangesTail(t *testing.T, stdout, want string) {
	t.Helper()
	if !strings.HasPrefix(stdout, want) {
		t.Fatalf("stdout = %q, want it to start with %q", stdout, want)
	}
	tail := strings.TrimPrefix(stdout, want)
	lines := strings.Split(strings.TrimSpace(tail), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "changes: ") || !strings.HasPrefix(lines[1], "outcome=") {
		t.Fatalf("stdout tail = %q, want one changes: line and one outcome= line", tail)
	}
}

// requireWritesTail asserts that human run output carries the
// worker-summary lines `want` verbatim, then exactly one change-manifest
// line and exactly one writes-check line. Both lines depend on the
// workspace's tree state at test time — "changes: 0 files, +0/-0" and
// "writes: ok" on a clean checkout, a dirty tree carrying measured
// counts and a writes verdict, the omitted and skipped forms only when
// measurement could not run — so only their presence and position after
// the summaries are pinned here; the checks' own tests pin their
// content.
func requireWritesTail(t *testing.T, stdout, want string) {
	t.Helper()
	if !strings.HasPrefix(stdout, want) {
		t.Fatalf("stdout = %q, want it to start with %q", stdout, want)
	}
	tail := strings.TrimPrefix(stdout, want)
	lines := strings.Split(strings.TrimSpace(tail), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "changes: ") || !strings.HasPrefix(lines[1], "writes: ") || !strings.HasPrefix(lines[2], "outcome=") {
		t.Fatalf("stdout tail = %q, want one changes: line, one writes: line, and one outcome= line", tail)
	}
}

func withBuildInfo(t *testing.T, version, commit, buildDate string) {
	t.Helper()
	oldVersion, oldCommit, oldBuildDate := buildinfo.Version, buildinfo.Commit, buildinfo.BuildDate
	buildinfo.Version, buildinfo.Commit, buildinfo.BuildDate = version, commit, buildDate
	t.Cleanup(func() {
		buildinfo.Version = oldVersion
		buildinfo.Commit = oldCommit
		buildinfo.BuildDate = oldBuildDate
	})
}

// installRealFakePiWorker points the newWorker seam at the fakepi test
// double so CLI cancellation tests exercise the real process lifecycle,
// reaping, and session-directory cleanup instead of the scripted stub.
func installRealFakePiWorker(t *testing.T) {
	t.Helper()
	original := newWorker
	newWorker = func() pi.Worker { return pi.New(fakePiBin) }
	t.Cleanup(func() { newWorker = original })
}

func installProcessVersionProbe(t *testing.T, output, childStderr string, exitCode int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "version.log")
	command := filepath.Join(dir, "pi")
	scriptText := "#!/bin/sh\nprintf '%s\\n' version >> \"$PI_WORKER_VERSION_LOG\"\nif [ -n \"${PI_WORKER_VERSION_MARKER:-}\" ]; then touch \"$PI_WORKER_VERSION_MARKER\"; fi\nprintf '%s' \"$PI_WORKER_VERSION_OUTPUT\"\nprintf '%s' \"${PI_WORKER_VERSION_STDERR:-}\" >&2\nexit \"$PI_WORKER_VERSION_EXIT\"\n"
	if err := os.WriteFile(command, []byte(scriptText), 0o700); err != nil {
		t.Fatalf("write version command: %v", err)
	}
	t.Setenv("PI_WORKER_VERSION_LOG", logPath)
	t.Setenv("PI_WORKER_VERSION_OUTPUT", output)
	t.Setenv("PI_WORKER_VERSION_STDERR", childStderr)
	t.Setenv("PI_WORKER_VERSION_EXIT", fmt.Sprintf("%d", exitCode))
	oldPath := os.Getenv("PATH")
	if oldPath == "" {
		t.Setenv("PATH", dir)
	} else {
		t.Setenv("PATH", dir+string(os.PathListSeparator)+oldPath)
	}
	original := runVersionProbe
	runVersionProbe = defaultRunVersionProbe
	t.Cleanup(func() { runVersionProbe = original })
	return logPath
}

func versionProbeCount(t *testing.T, logPath string) int {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read version probe log: %v", err)
	}
	return strings.Count(string(data), "version\n")
}

// setupFakePiScript writes a fakepi script and points FAKEPI_SCRIPT and
// FAKEPI_LOG at it.
func setupFakePiScript(t *testing.T, scriptConfig *script.Script) {
	t.Helper()
	dir := t.TempDir()
	if scriptConfig.Triggers == nil {
		scriptConfig.Triggers = make(map[string][]script.Step)
	}
	if _, hasTrigger := scriptConfig.Triggers["get_state"]; !hasTrigger && len(scriptConfig.TriggerSequences["get_state"]) == 0 {
		scriptConfig.Triggers["get_state"] = []script.Step{{Response: &script.Response{
			Success: true,
			Data:    json.RawMessage(`{"model":{"provider":"acme","id":"m-1"},"thinkingLevel":"medium","isStreaming":false}`),
		}}}
	}
	data, err := json.Marshal(scriptConfig)
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "script.json"), data, 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	t.Setenv("FAKEPI_SCRIPT", filepath.Join(dir, "script.json"))
	t.Setenv("FAKEPI_LOG", filepath.Join(dir, "requests.log"))
}

// sessionDirFromMeta reads the fakepi meta file and returns the
// --session-dir value from the recorded argv.
func sessionDirFromMeta(t *testing.T, metaPath string) string {
	t.Helper()
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read fakepi meta: %v", err)
	}
	var meta struct {
		Argv []string `json:"argv"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("decode fakepi meta: %v", err)
	}
	for i, arg := range meta.Argv {
		if arg == "--session-dir" && i+1 < len(meta.Argv) {
			return meta.Argv[i+1]
		}
	}
	t.Fatalf("fakepi meta argv has no --session-dir: %v", meta.Argv)
	return ""
}

// waitForRequestLog polls the fakepi request log until the given request
// type is recorded, proving the child is alive and mid-run.
func waitForRequestLog(t *testing.T, logPath, wantType string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if data, err := os.ReadFile(logPath); err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if strings.Contains(line, `"type":"`+wantType+`"`) {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("request log %s never recorded %q", logPath, wantType)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type runOutput struct {
	SchemaVersion int               `json:"schemaVersion"`
	Status        string            `json:"status"`
	Outcome       contracts.Outcome `json:"outcome"`
	Workers       []pi.WorkerResult `json:"workers"`
	Changes       *run.Changes      `json:"changes"`
}

// decodeRunOutput decodes the single --json result document from stdout.
func decodeRunOutput(t *testing.T, stdout string) runOutput {
	t.Helper()
	var output runOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("decode json stdout: %v (%q)", err, stdout)
	}
	return output
}

func mustWorkerRequest(t *testing.T, fake *fakeWorker, id int) pi.WorkerRequest {
	t.Helper()
	request, ok := fake.requestForWorker(id)
	if !ok {
		t.Fatalf("worker %d never invoked", id)
	}
	return request
}

func waitForWorkerCount(t *testing.T, fake *fakeWorker, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if fake.callCount() >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d workers started, want %d", fake.callCount(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForWorkerCompleted(t *testing.T, ch <-chan int, want int) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("worker %d completed first, want %d", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for worker %d to complete", want)
	}
}

func TestMainVersion(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Main([]string{"version"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if got := stdout.String(); got != "pi-worker dev\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestVersionCommandUsesInjectedIdentity(t *testing.T) {
	withBuildInfo(t, "v0.1.0", "0123456789abcdef0123456789abcdef01234567", "2026-08-11T00:00:00Z")
	code, stdout, stderr := runCLI(t, []string{"version"}, "")
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr = %q", code, stderr)
	}
	const want = "pi-worker v0.1.0 (commit 0123456789abcdef0123456789abcdef01234567, built 2026-08-11T00:00:00Z)\n"
	if got := stdout; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestVersionCommandWithContextUsesInjectedIdentity(t *testing.T) {
	withBuildInfo(t, "v0.1.0", "0123456789abcdef0123456789abcdef01234567", "2026-08-11T00:00:00Z")
	code, stdout, stderr := runCLIWithContext(t, context.Background(), []string{"version"}, "")
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr = %q", code, stderr)
	}
	const want = "pi-worker v0.1.0 (commit 0123456789abcdef0123456789abcdef01234567, built 2026-08-11T00:00:00Z)\n"
	if got := stdout; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestVersionJSONIsOneCompleteDocument(t *testing.T) {
	withBuildInfo(t, "v0.1.0", "0123456789abcdef0123456789abcdef01234567", "2026-08-11T00:00:00Z")
	code, stdout, stderr := runCLI(t, []string{"version", "--json"}, "")
	if code != 0 || stderr != "" || strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	var output struct {
		SchemaVersion int    `json:"schemaVersion"`
		Version       string `json:"version"`
		Commit        string `json:"commit"`
		BuildDate     string `json:"buildDate"`
	}
	decoder := json.NewDecoder(strings.NewReader(stdout))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&output); err != nil {
		t.Fatalf("decode version JSON: %v (%q)", err, stdout)
	}
	if output.SchemaVersion != 1 || output.Version != "v0.1.0" || output.Commit != "0123456789abcdef0123456789abcdef01234567" || output.BuildDate != "2026-08-11T00:00:00Z" {
		t.Fatalf("output = %#v", output)
	}
}

func TestVersionJSONRepresentsSourceBuildExplicitly(t *testing.T) {
	code, stdout, stderr := runCLIWithContext(t, context.Background(), []string{"version", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	const want = "{\"schemaVersion\":1,\"version\":\"dev\",\"commit\":\"unknown\",\"buildDate\":\"unknown\"}\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestVersionRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		{"version", "--unknown"},
		{"version", "--json", "--json"},
		{"version", "--json=true"},
		{"version", "extra"},
	} {
		code, stdout, stderr := runCLI(t, args, "")
		if code != 2 || stdout != "" || !strings.Contains(stderr, "pi-worker:") {
			t.Fatalf("args = %v, exit = %d, stdout = %q, stderr = %q", args, code, stdout, stderr)
		}
	}
}

func TestRunUsageErrors(t *testing.T) {
	installConfigPath(t, filepath.Join(t.TempDir(), "config.json"))
	emptyFile := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(emptyFile, []byte("   \n"), 0o600); err != nil {
		t.Fatalf("write empty file: %v", err)
	}
	tests := []struct {
		name  string
		args  []string
		stdin string
	}{
		{name: "unknown flag", args: []string{"run", "--model", "acme/m-1", "--bogus"}, stdin: ""},
		{name: "missing model", args: []string{"run"}, stdin: "do it"},
		{name: "model without value", args: []string{"run", "--model"}, stdin: ""},
		{name: "model without provider", args: []string{"run", "--model", "/m-1"}, stdin: ""},
		{name: "model without id", args: []string{"run", "--model", "acme/"}, stdin: ""},
		{name: "model with thinking suffix", args: []string{"run", "--model", "acme/m-1:thinking"}, stdin: ""},
		{name: "model pattern", args: []string{"run", "--model", "acme*"}, stdin: ""},
		{name: "empty task", args: []string{"run", "--model", "acme/m-1", "--task", ""}, stdin: ""},
		{name: "task and task file", args: []string{"run", "--model", "acme/m-1", "--task", "a", "--task-file", "b.txt"}, stdin: ""},
		{name: "repeated model", args: []string{"run", "--model", "acme/m-1", "--model", "acme/m-2"}, stdin: ""},
		{name: "thinking without value", args: []string{"run", "--model", "acme/m-1", "--thinking"}, stdin: ""},
		{name: "empty thinking", args: []string{"run", "--model", "acme/m-1", "--thinking=", "--task", "a"}, stdin: ""},
		{name: "invalid thinking", args: []string{"run", "--model", "acme/m-1", "--thinking", "ultra", "--task", "a"}, stdin: ""},
		{name: "mixed-case thinking", args: []string{"run", "--model", "acme/m-1", "--thinking", "MAX", "--task", "a"}, stdin: ""},
		{name: "repeated thinking", args: []string{"run", "--model", "acme/m-1", "--thinking", "low", "--thinking", "max", "--task", "a"}, stdin: ""},
		{name: "repeated timeout", args: []string{"run", "--model", "acme/m-1", "--timeout", "1m", "--timeout", "2m"}, stdin: ""},
		{name: "repeated json", args: []string{"run", "--model", "acme/m-1", "--json", "--json"}, stdin: ""},
		{name: "debug with value", args: []string{"run", "--model", "acme/m-1", "--debug=true"}, stdin: ""},
		{name: "repeated debug", args: []string{"run", "--model", "acme/m-1", "--debug", "--debug"}, stdin: ""},
		{name: "invalid timeout", args: []string{"run", "--model", "acme/m-1", "--timeout", "soon"}, stdin: ""},
		{name: "zero timeout", args: []string{"run", "--model", "acme/m-1", "--timeout", "0s"}, stdin: ""},
		{name: "negative timeout", args: []string{"run", "--model", "acme/m-1", "--timeout", "-1m"}, stdin: ""},
		{name: "timeout without value", args: []string{"run", "--model", "acme/m-1", "--timeout"}, stdin: ""},
		{name: "json with value", args: []string{"run", "--model", "acme/m-1", "--json=true"}, stdin: ""},
		{name: "positional argument", args: []string{"run", "--model", "acme/m-1", "extra"}, stdin: ""},
		{name: "empty stdin", args: []string{"run", "--model", "acme/m-1"}, stdin: ""},
		{name: "empty task file", args: []string{"run", "--model", "acme/m-1", "--task-file", emptyFile}, stdin: ""},
		{name: "too many tasks", args: []string{"run", "--model", "acme/m-1", "--task", "a", "--task", "b", "--task", "c", "--task", "d"}, stdin: ""},
		{name: "too many task files", args: []string{"run", "--model", "acme/m-1", "--task-file", "a", "--task-file", "b", "--task-file", "c", "--task-file", "d"}, stdin: ""},
		{name: "writes before multiple tasks", args: []string{"run", "--model", "acme/m-1", "--writes", "src/a", "--task", "a", "--task", "b"}, stdin: ""},
		{name: "repeated writes for one task", args: []string{"run", "--model", "acme/m-1", "--task", "a", "--writes", "src/a", "--writes", "src/b"}, stdin: ""},
		{name: "repeated writes for one task file", args: []string{"run", "--model", "acme/m-1", "--task-file", "a", "--writes", "src/a", "--writes", "src/b"}, stdin: ""},
		{name: "empty writes element", args: []string{"run", "--model", "acme/m-1", "--task", "a", "--writes", "src/a,,src/b"}, stdin: ""},
		{name: "whitespace-only writes element", args: []string{"run", "--model", "acme/m-1", "--task", "a", "--writes", "src/a, "}, stdin: ""},
		{name: "absolute writes path", args: []string{"run", "--model", "acme/m-1", "--task", "a", "--writes", "/etc/passwd"}, stdin: ""},
		{name: "writes path escapes workspace", args: []string{"run", "--model", "acme/m-1", "--task", "a", "--writes", "../outside"}, stdin: ""},
		{name: "writes whole workspace dot", args: []string{"run", "--model", "acme/m-1", "--task", "a", "--writes", "."}, stdin: ""},
		{name: "writes without value", args: []string{"run", "--model", "acme/m-1", "--task", "a", "--writes"}, stdin: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
			code, stdout, stderr := runCLI(t, test.args, test.stdin)
			if code != 2 {
				t.Fatalf("exit = %d, want 2; stderr = %q", code, stderr)
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want empty", stdout)
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

func TestRunUsageShowsWritesFlag(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Main([]string{}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--writes <paths>") {
		t.Fatalf("usage does not mention --writes: %q", stderr.String())
	}
}

func TestRunWritesSuppressesSharedWorkspaceWarningWhenEveryTaskDeclares(t *testing.T) {
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	fake.resultsByWorker = map[int]pi.WorkerResult{
		1: {Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "one done"},
		2: {Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "two done"},
	}
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "one", "--writes", "src/a", "--task", "two", "--writes", "src/b"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if strings.Contains(stderr, "share the writable current workspace") {
		t.Fatalf("stderr printed the shared-workspace warning: %q", stderr)
	}
	requireWritesTail(t, stdout, "worker 1: one done\nworker 2: two done\n")
	if fake.callCount() != 2 {
		t.Fatalf("worker calls = %d, want 2", fake.callCount())
	}
}

func TestRunWritesKeepsSharedWorkspaceWarningWhenAnyTaskDeclaresNothing(t *testing.T) {
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	fake.resultsByWorker = map[int]pi.WorkerResult{
		1: {Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "one done"},
		2: {Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "two done"},
	}
	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "one", "--writes", "src/a", "--task", "two"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	// The --task two worker declared nothing, so the run is not fully
	// contracted and the warning must stay.
	if count := strings.Count(stderr, "pi-worker: warning: 2 workers share the writable current workspace; tasks must use disjoint files"); count != 1 {
		t.Fatalf("warning count = %d, want 1: %q", count, stderr)
	}
	if fake.callCount() != 2 {
		t.Fatalf("worker calls = %d, want 2", fake.callCount())
	}
}

func TestRunWritesWithTaskFilesSuppressesWarning(t *testing.T) {
	firstPath := filepath.Join(t.TempDir(), "task-1.txt")
	secondPath := filepath.Join(t.TempDir(), "task-2.txt")
	if err := os.WriteFile(firstPath, []byte("first task"), 0o600); err != nil {
		t.Fatalf("write task file: %v", err)
	}
	if err := os.WriteFile(secondPath, []byte("second task"), 0o600); err != nil {
		t.Fatalf("write task file: %v", err)
	}
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	fake.resultsByWorker = map[int]pi.WorkerResult{
		1: {Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "first file done"},
		2: {Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "second file done"},
	}
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task-file", firstPath, "--writes", "internal/run,docs/a.md", "--task-file", secondPath, "--writes", "internal/cli"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if strings.Contains(stderr, "share the writable current workspace") {
		t.Fatalf("stderr printed the shared-workspace warning: %q", stderr)
	}
	requireWritesTail(t, stdout, "worker 1: first file done\nworker 2: second file done\n")
	if req := mustWorkerRequest(t, fake, 1); req.Prompt != "first task" {
		t.Fatalf("worker 1 prompt = %q, want first task", req.Prompt)
	}
	if req := mustWorkerRequest(t, fake, 2); req.Prompt != "second task" {
		t.Fatalf("worker 2 prompt = %q, want second task", req.Prompt)
	}
}

func TestRunWritesOverlapRejectedBeforeAnyWorkerStarts(t *testing.T) {
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "one", "--writes", "src/a", "--task", "two", "--writes", "src/a/b.go"}, "")
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %q", code, stderr)
	}
	if fake.callCount() != 0 {
		t.Fatalf("worker invoked %d times before rejection", fake.callCount())
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty for rejected run", stdout)
	}
	// Escaping 9 is the point: the declaration is a usage error like any
	// other argv mistake, so the usage block is printed along with it.
	if !strings.Contains(stderr, "usage:") {
		t.Fatalf("stderr missing usage text: %q", stderr)
	}
	if !strings.Contains(stderr, `pi-worker: task 1 and task 2 declare overlapping write paths "src/a" and "src/a/b.go"`) {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestRunWritesWhitespaceAroundCommasDoesNotDefeatOverlapCheck(t *testing.T) {
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	// "docs/a.md, src/x" must reach validation as the same paths as
	// "docs/a.md,src/x": the space after the comma is formatting, not part
	// of the path, so the overlap with task two's "src/x" is still rejected
	// before any worker starts, as a usage error exiting 2.
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "one", "--writes", "docs/a.md, src/x", "--task", "two", "--writes", "src/x"}, "")
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %q", code, stderr)
	}
	if fake.callCount() != 0 {
		t.Fatalf("worker invoked %d times before rejection", fake.callCount())
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty for rejected run", stdout)
	}
	want := `pi-worker: task 1 and task 2 declare overlapping write paths "src/x" and "src/x"`
	if !strings.Contains(stderr, want) {
		t.Fatalf("stderr = %q, want it to contain %q", stderr, want)
	}
	// The surrounding whitespace must not leak into the reported path.
	if strings.Contains(stderr, `" src/x"`) {
		t.Fatalf("stderr reports the untrimmed path: %q", stderr)
	}
}

func TestRunRejectedWritesDeclarationsExitTwoWithUnchangedControllerMessage(t *testing.T) {
	// A bad --writes declaration used to fail the controller's validate
	// and exit 9 as an internal failure. The CLI now validates the
	// declaration in resolveRunInput, so the rejection is a usage error
	// exiting 2 like every other argv mistake. The message text is the
	// controller's message verbatim and is pinned here, so a future edit
	// cannot quietly reword it while only changing the exit path.
	tests := []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{
			name:       "absolute path",
			args:       []string{"run", "--model", "acme/m-1", "--task", "a", "--writes", "/etc/passwd"},
			wantStderr: `pi-worker: task 1: write path "/etc/passwd" is absolute; declare paths relative to the workspace`,
		},
		{
			name:       "path escapes workspace",
			args:       []string{"run", "--model", "acme/m-1", "--task", "a", "--writes", "../outside"},
			wantStderr: `pi-worker: task 1: write path "../outside" escapes the workspace`,
		},
		{
			name:       "whole workspace dot",
			args:       []string{"run", "--model", "acme/m-1", "--task", "a", "--writes", "."},
			wantStderr: `pi-worker: task 1: write path "." declares the whole workspace`,
		},
		{
			name:       "same path twice in one task",
			args:       []string{"run", "--model", "acme/m-1", "--task", "a", "--writes", "src/a,src/a"},
			wantStderr: `pi-worker: task 1 declares write path "src/a" more than once`,
		},
		{
			name:       "overlap between tasks",
			args:       []string{"run", "--model", "acme/m-1", "--task", "one", "--writes", "src/a", "--task", "two", "--writes", "src/a/b.go"},
			wantStderr: `pi-worker: task 1 and task 2 declare overlapping write paths "src/a" and "src/a/b.go"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
			code, stdout, stderr := runCLI(t, test.args, "")
			if code != 2 {
				t.Fatalf("exit = %d, want 2; stderr = %q", code, stderr)
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want empty", stdout)
			}
			// Usage errors from resolveRunInput print the usage block; the
			// write-declaration rejection must behave exactly like them.
			if !strings.Contains(stderr, "usage:") {
				t.Fatalf("stderr missing usage text: %q", stderr)
			}
			if !strings.Contains(stderr, test.wantStderr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr, test.wantStderr)
			}
			if fake.callCount() != 0 {
				t.Fatalf("worker invoked %d times before rejection", fake.callCount())
			}
		})
	}
}

func TestParseRunArgsEmptyWritesDeclaresEmptySet(t *testing.T) {
	// --writes "" is the one spelling that cannot collide with a real
	// path: it is how a task declares that it writes nothing. The same
	// whitespace trimming the flag already applies to real paths must
	// apply here, so --writes "  " means the same thing. Every other
	// parse failure keeps failing exactly as before.
	for _, value := range []string{"", "   "} {
		for _, args := range [][]string{
			{"--task", "a", "--writes", value},
			{"--task", "a", "--writes=" + value},
		} {
			opts, err := parseRunArgs(args)
			if err != nil {
				t.Fatalf("parseRunArgs(%q): %v, want a declared empty set", args, err)
			}
			if len(opts.writes) != 1 || !opts.writes[0].Declared || len(opts.writes[0].Paths) != 0 {
				t.Fatalf("writes = %#v, want one declared empty entry for %q", opts.writes, args)
			}
		}
	}
	if _, err := parseRunArgs([]string{"--task", "a", "--writes", "a,,b"}); err == nil {
		t.Fatalf("parseRunArgs accepted --writes \"a,,b\", want the empty-element error")
	}
	if _, err := parseRunArgs([]string{"--task", "a", "--writes", "src/a", "--writes", "src/b"}); err == nil {
		t.Fatalf("parseRunArgs accepted a repeated --writes, want the duplicate error")
	}
	if _, err := parseRunArgs([]string{"--task", "a", "--writes", "", "--writes", "src/a"}); err == nil {
		t.Fatalf("parseRunArgs accepted a repeated --writes after an empty declaration, want the duplicate error")
	}
	// A --writes before any task is no longer a parse error: with
	// exactly one task — a flag or a stdin prompt — its target is
	// unambiguous, and the decision is deferred to resolveRunInput, where
	// the final task count is known.
	opts, err := parseRunArgs([]string{"--writes", "src/a"})
	if err != nil {
		t.Fatalf("parseRunArgs rejected a leading --writes: %v", err)
	}
	if len(opts.writesPending) != 1 || len(opts.writesPending[0].Paths) != 1 || opts.writesPending[0].Paths[0] != "src/a" {
		t.Fatalf("writesPending = %#v, want the pending src/a declaration", opts.writesPending)
	}
	if _, err := parseRunArgs([]string{"--writes", ""}); err != nil {
		t.Fatalf("parseRunArgs rejected a leading --writes \"\": %v", err)
	}
}

func TestRunWritesBeforeSingleTaskReachesThatTask(t *testing.T) {
	// With exactly one task the ordering rule carries no information, so
	// a --writes placed before the --task or --task-file is that task's
	// declaration and must reach it. The "writes: ok" verdict on a clean
	// workspace only prints when the declaration was bound; one that
	// reached no task skips the check instead.
	taskFile := filepath.Join(t.TempDir(), "task.txt")
	if err := os.WriteFile(taskFile, []byte("go"), 0o600); err != nil {
		t.Fatalf("write task file: %v", err)
	}
	newGitWorkspace(t)
	installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "done"})
	for _, args := range [][]string{
		{"run", "--model", "acme/m-1", "--writes", "file.txt", "--task", "go"},
		{"run", "--model", "acme/m-1", "--writes", "file.txt", "--task-file", taskFile},
	} {
		code, stdout, stderr := runCLI(t, args, "")
		if code != 0 {
			t.Fatalf("%v: exit = %d, want 0; stderr = %q", args, code, stderr)
		}
		if stderr != "" {
			t.Fatalf("%v: stderr = %q", args, stderr)
		}
		const want = "worker 1: done\n" +
			"changes: 0 files, +0/-0\n" +
			"writes: ok\n" +
			"outcome=completed\n"
		if stdout != want {
			t.Fatalf("%v: stdout = %q, want %q", args, stdout, want)
		}
	}
}

func TestRunWritesWithStdinPromptReachesTheStdinTask(t *testing.T) {
	// A prompt on stdin has no --task flag for a positional --writes to
	// follow, so the single-task rule is the only way the documented
	// feature can be used at all in this input mode: the declaration
	// must bind to the stdin task.
	newGitWorkspace(t)
	fake := installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "done"})
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--writes", "file.txt"}, "do it")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q", stderr)
	}
	const want = "worker 1: done\n" +
		"changes: 0 files, +0/-0\n" +
		"writes: ok\n" +
		"outcome=completed\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	if req := mustWorkerRequest(t, fake, 1); req.Prompt != "do it" {
		t.Fatalf("worker prompt = %q, want the stdin prompt", req.Prompt)
	}
}

func TestRunWritesBeforeMultipleTasksRejectedWithRemedy(t *testing.T) {
	// A --writes that precedes more than one task has no knowable
	// target, so the run stays rejected — but the message must say what
	// to do, not only what is wrong: the declaration has to name its
	// task by following it.
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--writes", "src/a", "--task", "one", "--task", "two"}, "")
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %q", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "--writes must follow the --task or --task-file it declares") {
		t.Fatalf("stderr missing the ambiguous-target error: %q", stderr)
	}
	if !strings.Contains(stderr, "place each --writes directly after its task") {
		t.Fatalf("stderr missing the remedy: %q", stderr)
	}
	if fake.callCount() != 0 {
		t.Fatalf("worker invoked %d times before rejection", fake.callCount())
	}
}

func TestRunWritesTwiceForSingleTaskStillRejectedInAnyPosition(t *testing.T) {
	// The once-per-task limit is unchanged: a declaration that arrives
	// twice for the same task is rejected with the existing error,
	// whatever positions the two occurrences took — pending colliding
	// with positional, two pendings, and two pendings around a stdin
	// prompt.
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	for _, test := range []struct {
		args  []string
		stdin string
	}{
		{args: []string{"run", "--model", "acme/m-1", "--writes", "src/a", "--task", "one", "--writes", "src/b"}},
		{args: []string{"run", "--model", "acme/m-1", "--writes", "src/a", "--writes", "src/b", "--task", "one"}},
		{args: []string{"run", "--model", "acme/m-1", "--writes", "src/a", "--writes", "src/b"}, stdin: "do it"},
	} {
		code, stdout, stderr := runCLI(t, test.args, test.stdin)
		if code != 2 {
			t.Fatalf("%v: exit = %d, want 2; stderr = %q", test.args, code, stderr)
		}
		if stdout != "" {
			t.Fatalf("%v: stdout = %q, want empty", test.args, stdout)
		}
		if !strings.Contains(stderr, "--writes specified more than once for task 1") {
			t.Fatalf("%v: stderr = %q, want the more-than-once error", test.args, stderr)
		}
	}
	if fake.callCount() != 0 {
		t.Fatalf("worker invoked %d times before rejection", fake.callCount())
	}
}

func TestRunWritesNothingDeclarationAcceptedBeforeSingleTask(t *testing.T) {
	// --writes "" behaves identically wherever it appears: placed before
	// the single task it must still declare the writes-nothing set,
	// which the "writes: ok" verdict on a clean workspace proves.
	newGitWorkspace(t)
	installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "done"})
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--writes", "", "--task", "go"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q", stderr)
	}
	const want = "worker 1: done\n" +
		"changes: 0 files, +0/-0\n" +
		"writes: ok\n" +
		"outcome=completed\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestRunWritesEmptySetSuppressesSharedWorkspaceWarning(t *testing.T) {
	// A task that declared --writes "" has declared: the run is fully
	// contracted, so the shared-workspace warning must stay suppressed
	// even though that task declared no paths at all.
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	fake.resultsByWorker = map[int]pi.WorkerResult{
		1: {Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "one done"},
		2: {Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "two done"},
	}
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "one", "--writes", "src/a", "--task", "two", "--writes", ""}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if strings.Contains(stderr, "share the writable current workspace") {
		t.Fatalf("stderr printed the shared-workspace warning: %q", stderr)
	}
	requireWritesTail(t, stdout, "worker 1: one done\nworker 2: two done\n")
	if fake.callCount() != 2 {
		t.Fatalf("worker calls = %d, want 2", fake.callCount())
	}
}

func TestRunSuccessHuman(t *testing.T) {
	fake := installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "All done."})
	// Anchor before the run so the 30-minute default is measured from a
	// fixed reference instead of decaying toward "now" while the run
	// executes.
	start := time.Now()
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "fix the bug"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	requireChangesTail(t, stdout, "worker 1: All done.\n")
	if stderr != "" {
		t.Fatalf("stderr = %q", stderr)
	}
	request := mustWorkerRequest(t, fake, 1)
	if request.Model != "acme/m-1" || request.Prompt != "fix the bug" {
		t.Fatalf("request = %#v", request)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if request.Workspace != cwd {
		t.Fatalf("workspace = %q, want %q", request.Workspace, cwd)
	}
	deadline, ok := fake.deadlineForWorker(1)
	if !ok {
		t.Fatalf("worker 1 had no deadline")
	}
	fromStart := deadline.Sub(start)
	if fromStart < 29*time.Minute || fromStart > 31*time.Minute {
		t.Fatalf("default deadline is %v from run start, want about 30m", fromStart)
	}
}

func TestRunThinkingPropagatesAndLabelsHumanOutput(t *testing.T) {
	fake := installFakeWorker(t, pi.WorkerResult{
		Model:                  "acme/m-1",
		RequestedThinkingLevel: pi.ThinkingMax,
		ThinkingLevel:          pi.ThinkingMax,
		Status:                 pi.StatusCompleted,
		Explanation:            "All done.",
	})
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--thinking=max", "--task", "fix the bug"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	requireChangesTail(t, stdout, "worker 1 [model=acme/m-1 thinking=max]: All done.\n")
	request := mustWorkerRequest(t, fake, 1)
	if request.ThinkingLevel != pi.ThinkingMax {
		t.Fatalf("thinking = %q, want max", request.ThinkingLevel)
	}
}

func TestRunAcceptsEveryDocumentedThinkingLevel(t *testing.T) {
	for _, level := range []pi.ThinkingLevel{
		pi.ThinkingOff,
		pi.ThinkingMinimal,
		pi.ThinkingLow,
		pi.ThinkingMedium,
		pi.ThinkingHigh,
		pi.ThinkingXHigh,
		pi.ThinkingMax,
	} {
		t.Run(string(level), func(t *testing.T) {
			fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted, Explanation: "done"})
			code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--thinking", string(level), "--task", "go"}, "")
			if code != 0 || stderr != "" {
				t.Fatalf("exit = %d, stderr = %q", code, stderr)
			}
			if got := mustWorkerRequest(t, fake, 1).ThinkingLevel; got != level {
				t.Fatalf("thinking = %q, want %q", got, level)
			}
		})
	}
}

func TestRunPerTaskModelsReachOwnWorkers(t *testing.T) {
	// Three tasks, three different models, one run: every worker
	// receives its own task's model, asserted per worker rather than in
	// aggregate.
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	fake.resultsByWorker = map[int]pi.WorkerResult{
		1: {Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "one done"},
		2: {Model: "acme/m-2", Status: pi.StatusCompleted, Explanation: "two done"},
		3: {Model: "acme/m-3", Status: pi.StatusCompleted, Explanation: "three done"},
	}
	code, stdout, stderr := runCLI(t, []string{"run", "--task", "one", "--model", "acme/m-1", "--task", "two", "--model", "acme/m-2", "--task", "three", "--model", "acme/m-3"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	requireChangesTail(t, stdout, "worker 1: one done\nworker 2: two done\nworker 3: three done\n")
	for i, want := range []string{"acme/m-1", "acme/m-2", "acme/m-3"} {
		if req := mustWorkerRequest(t, fake, i+1); req.Model != want {
			t.Fatalf("worker %d model = %q, want %q", i+1, req.Model, want)
		}
	}
}

func TestRunTaskThinkingLevelsDoNotLeak(t *testing.T) {
	// Two tasks on the same model at different thinking levels: the
	// levels bind to their own tasks and do not leak into one another.
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	fake.resultsByWorker = map[int]pi.WorkerResult{
		1: {Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "one done"},
		2: {Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "two done"},
	}
	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "one", "--thinking", "low", "--task", "two", "--thinking", "max"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if got := mustWorkerRequest(t, fake, 1).ThinkingLevel; got != pi.ThinkingLow {
		t.Fatalf("worker 1 thinking = %q, want low", got)
	}
	if got := mustWorkerRequest(t, fake, 2).ThinkingLevel; got != pi.ThinkingMax {
		t.Fatalf("worker 2 thinking = %q, want max", got)
	}
}

func TestRunTaskWithoutModelFallsBackToRunLevelModel(t *testing.T) {
	// A task with no --model of its own falls back to the run-level
	// --model; a task with its own keeps it.
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	fake.resultsByWorker = map[int]pi.WorkerResult{
		1: {Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "one done"},
		2: {Model: "acme/m-2", Status: pi.StatusCompleted, Explanation: "two done"},
	}
	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "one", "--task", "two", "--model", "acme/m-2"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if got := mustWorkerRequest(t, fake, 1).Model; got != "acme/m-1" {
		t.Fatalf("worker 1 model = %q, want the run-level acme/m-1", got)
	}
	if got := mustWorkerRequest(t, fake, 2).Model; got != "acme/m-2" {
		t.Fatalf("worker 2 model = %q, want its own acme/m-2", got)
	}
}

func TestRunTaskThinkingOffStaysOff(t *testing.T) {
	// A task --thinking of "off" is an explicit level, not unset: it
	// must not fall back to the run-level thinking.
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted, Explanation: "done"})
	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--thinking", "max", "--task", "go", "--thinking", "off"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if got := mustWorkerRequest(t, fake, 1).ThinkingLevel; got != pi.ThinkingOff {
		t.Fatalf("thinking = %q, want off", got)
	}
}

func TestRunModelBeforeTasksIsRunLevelAcrossTasks(t *testing.T) {
	// A --model that precedes every task keeps its run-level meaning on
	// a multi-task run: every worker runs with it.
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	fake.resultsByWorker = map[int]pi.WorkerResult{
		1: {Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "one done"},
		2: {Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "two done"},
	}
	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "one", "--task", "two"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	for i := 1; i <= 2; i++ {
		if got := mustWorkerRequest(t, fake, i).Model; got != "acme/m-1" {
			t.Fatalf("worker %d model = %q, want run-level acme/m-1", i, got)
		}
	}
}

func TestRunSecondModelForSameTaskRejectedBeforeAnyWorkerStarts(t *testing.T) {
	// A second --model bound to the same task is rejected with the
	// task-naming error, before any worker starts.
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "one", "--model", "acme/m-2", "--model", "acme/m-3"}, "")
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %q", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "--model specified more than once for task 1") {
		t.Fatalf("stderr = %q, want the more-than-once error", stderr)
	}
	if fake.callCount() != 0 {
		t.Fatalf("worker invoked %d times before rejection", fake.callCount())
	}
}

func TestRunInvalidPerTaskModelUsesRunLevelErrorText(t *testing.T) {
	// An invalid per-task model is rejected with the same error text as
	// an invalid run-level one, before any worker starts.
	tests := []struct {
		name  string
		value string
	}{
		{name: "no provider", value: "/m-1"},
		{name: "no id", value: "acme/"},
		{name: "thinking suffix", value: "acme/m-1:thinking"},
		{name: "pattern", value: "acme*"},
	}
	errorLine := func(stderr string) string {
		for _, line := range strings.Split(stderr, "\n") {
			if strings.HasPrefix(line, "pi-worker: ") {
				return line
			}
		}
		return ""
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runLevel := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
			_, _, runLevelStderr := runCLI(t, []string{"run", "--model", test.value, "--task", "go"}, "")
			perTask := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
			code, stdout, taskStderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--model", test.value}, "")
			if code != 2 || stdout != "" {
				t.Fatalf("invalid per-task model = (%d, %q, %q)", code, stdout, taskStderr)
			}
			if runLevel.callCount() != 0 || perTask.callCount() != 0 {
				t.Fatalf("worker invoked for an invalid model")
			}
			if !strings.Contains(taskStderr, "invalid model") {
				t.Fatalf("per-task stderr = %q, want the invalid-model error", taskStderr)
			}
			if want, got := errorLine(runLevelStderr), errorLine(taskStderr); got != want {
				t.Fatalf("per-task error line = %q, want the run-level line %q", got, want)
			}
		})
	}
}

func TestRunThinkingFallbackWarnsAndKeepsSuccessfulExit(t *testing.T) {
	warning := "requested thinking=max unavailable; continuing with Pi default thinking=medium"
	result := pi.WorkerResult{
		Model:                  "acme/m-1",
		RequestedThinkingLevel: pi.ThinkingMax,
		ThinkingLevel:          pi.ThinkingMedium,
		ThinkingFallback:       true,
		Warning:                warning,
		Status:                 pi.StatusCompleted,
		Explanation:            "Completed with default effort.",
	}
	_ = installFakeWorker(t, result)

	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--thinking", "max", "--task", "go", "--json"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if stderr != "pi-worker: worker 1: "+warning+"\n" {
		t.Fatalf("stderr = %q", stderr)
	}
	output := decodeRunOutput(t, stdout)
	if len(output.Workers) != 1 || !output.Workers[0].ThinkingFallback || output.Workers[0].ThinkingLevel != pi.ThinkingMedium || output.Workers[0].Warning != warning {
		t.Fatalf("output = %#v", output)
	}
}

func TestRunSuccessJSON(t *testing.T) {
	fake := installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "JSON answer"})
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--json"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q", stderr)
	}
	if strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
		t.Fatalf("stdout has multiple objects: %q", stdout)
	}
	output := decodeRunOutput(t, stdout)
	if output.SchemaVersion != 1 {
		t.Fatalf("schemaVersion = %d, want 1", output.SchemaVersion)
	}
	if output.Status != "completed" {
		t.Fatalf("status = %q", output.Status)
	}
	if len(output.Workers) != 1 {
		t.Fatalf("worker count = %d, want 1", len(output.Workers))
	}
	if output.Workers[0].Model != "acme/m-1" || output.Workers[0].Explanation != "JSON answer" || output.Workers[0].Status != "completed" {
		t.Fatalf("worker = %#v", output.Workers[0])
	}
	_ = fake
}

func TestRunVerifiedPiVersionProbesOnceBeforeWorkers(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "version-probed")
	logPath := installProcessVersionProbe(t, "0.84.2\n", "", 0)
	t.Setenv("PI_WORKER_VERSION_MARKER", marker)
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted, Explanation: "ok"})
	fake.runHook = func() {
		if _, err := os.Stat(marker); err != nil {
			t.Errorf("worker started before version probe: %v", err)
		}
	}

	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "one", "--task", "two", "--task", "three"}, "")
	if code != 0 || stdout == "" || strings.Contains(stderr, "Pi version") {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if got := versionProbeCount(t, logPath); got != 1 {
		t.Fatalf("version probe count = %d, want 1", got)
	}
	if fake.callCount() != 3 {
		t.Fatalf("worker calls = %d, want 3", fake.callCount())
	}
}

func TestRunUnverifiedPiVersionWarnsOnceAndKeepsJSONClean(t *testing.T) {
	logPath := installProcessVersionProbe(t, "0.99.0\n", "child-secret-must-not-leak", 0)
	installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "JSON answer"})

	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--json"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if got := versionProbeCount(t, logPath); got != 1 {
		t.Fatalf("version probe count = %d, want 1", got)
	}
	const wantWarning = "pi-worker: warning: Pi version 0.99.0 is unverified; verified version is 0.84.2; continuing\n"
	if stderr != wantWarning || strings.Contains(stderr, "child-secret-must-not-leak") {
		t.Fatalf("stderr = %q", stderr)
	}
	_ = decodeRunOutput(t, stdout)
	if strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
		t.Fatalf("stdout has multiple JSON documents: %q", stdout)
	}
}

func TestRunMalformedPiVersionWarnsAndKeepsJSONClean(t *testing.T) {
	installProcessVersionProbe(t, "pi 0.84.1\n", "", 0)
	installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "ok"})

	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--json"}, "")
	if code != 0 || strings.Count(stderr, "pi-worker: warning: Pi version") != 1 {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	_ = decodeRunOutput(t, stdout)
	if strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
		t.Fatalf("stdout has multiple JSON documents: %q", stdout)
	}
}

func TestRunPiVersionProbeFailureKeepsExistingExitCode(t *testing.T) {
	installProcessVersionProbe(t, "probe-output-secret", "child-stderr-secret", 7)
	installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusUnavailable, Error: "model unavailable"})

	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--json"}, "")
	if code != 3 {
		t.Fatalf("exit = %d, want existing readiness exit 3; stderr = %q", code, stderr)
	}
	_ = decodeRunOutput(t, stdout)
	if !strings.Contains(stderr, "pi-worker: warning: Pi version") || !strings.Contains(stderr, "model unavailable") {
		t.Fatalf("stderr = %q", stderr)
	}
	if strings.Contains(stderr, "probe-output-secret") || strings.Contains(stderr, "child-stderr-secret") {
		t.Fatalf("stderr leaked probe output: %q", stderr)
	}
}

func TestRunTaskFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.txt")
	if err := os.WriteFile(path, []byte("fix from file"), 0o600); err != nil {
		t.Fatalf("write task file: %v", err)
	}
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted, Explanation: "ok"})
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task-file", path}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	requireChangesTail(t, stdout, "worker 1: ok\n")
	request := mustWorkerRequest(t, fake, 1)
	if request.Prompt != "fix from file" {
		t.Fatalf("prompt = %q", request.Prompt)
	}
}

func TestRunStdinTask(t *testing.T) {
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted, Explanation: "ok"})
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1"}, "task from stdin")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	requireChangesTail(t, stdout, "worker 1: ok\n")
	request := mustWorkerRequest(t, fake, 1)
	if request.Prompt != "task from stdin" {
		t.Fatalf("prompt = %q", request.Prompt)
	}
}

func TestRunThreeTasksHumanSuccessIsOrderedAndConcurrent(t *testing.T) {
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	fake.resultsByWorker = map[int]pi.WorkerResult{
		1: {Status: pi.StatusCompleted, Model: "acme/m-1", Explanation: "first done"},
		2: {Status: pi.StatusCompleted, Model: "acme/m-1", Explanation: "second done"},
		3: {Status: pi.StatusCompleted, Model: "acme/m-1", Explanation: "third done"},
	}
	fake.startGate = make(chan struct{})
	fake.startGateAt = 3
	fake.releaseByWorker = map[int]chan struct{}{
		1: make(chan struct{}),
		2: make(chan struct{}),
		3: make(chan struct{}),
	}
	fake.completed = make(chan int, 3)

	var code int
	var stdout, stderr string
	done := make(chan struct{})
	go func() {
		defer close(done)
		code, stdout, stderr = runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "one", "--task", "two", "--task", "three"}, "")
	}()
	waitForWorkerCount(t, fake, 3)
	close(fake.releaseByWorker[3])
	waitForWorkerCompleted(t, fake.completed, 3)
	close(fake.releaseByWorker[2])
	waitForWorkerCompleted(t, fake.completed, 2)
	close(fake.releaseByWorker[1])
	waitForWorkerCompleted(t, fake.completed, 1)
	<-done

	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if fake.maxConcurrency() != 3 {
		t.Fatalf("max concurrency = %d, want 3", fake.maxConcurrency())
	}
	if count := strings.Count(stderr, "pi-worker: warning:"); count != 1 {
		t.Fatalf("stderr warning count = %d, want 1", count)
	}
	requireChangesTail(t, stdout, "worker 1: first done\nworker 2: second done\nworker 3: third done\n")
	requestOrder := []string{"one", "two", "three"}
	for i, want := range requestOrder {
		req := mustWorkerRequest(t, fake, i+1)
		if req.Prompt != want {
			t.Fatalf("worker %d prompt = %q, want %q", i+1, req.Prompt, want)
		}
	}
}

func TestRunRepeatedTaskFilesPreserveInputOrder(t *testing.T) {
	firstPath := filepath.Join(t.TempDir(), "task-1.txt")
	secondPath := filepath.Join(t.TempDir(), "task-2.txt")
	if err := os.WriteFile(firstPath, []byte("first task"), 0o600); err != nil {
		t.Fatalf("write task file: %v", err)
	}
	if err := os.WriteFile(secondPath, []byte("second task"), 0o600); err != nil {
		t.Fatalf("write task file: %v", err)
	}

	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	fake.resultsByWorker = map[int]pi.WorkerResult{
		1: {Status: pi.StatusCompleted, Model: "acme/m-1", Explanation: "first file done"},
		2: {Status: pi.StatusCompleted, Model: "acme/m-1", Explanation: "second file done"},
	}
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task-file", firstPath, "--task-file", secondPath}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if count := strings.Count(stderr, "pi-worker: warning:"); count != 1 {
		t.Fatalf("stderr warning count = %d, want 1", count)
	}
	requireChangesTail(t, stdout, "worker 1: first file done\nworker 2: second file done\n")
	if req := mustWorkerRequest(t, fake, 1); req.Prompt != "first task" {
		t.Fatalf("worker 1 prompt = %q, want first task", req.Prompt)
	}
	if req := mustWorkerRequest(t, fake, 2); req.Prompt != "second task" {
		t.Fatalf("worker 2 prompt = %q, want second task", req.Prompt)
	}
}

func TestRunTwoTaskJSONResultOrder(t *testing.T) {
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	fake.resultsByWorker = map[int]pi.WorkerResult{
		1: {Status: pi.StatusCompleted, Model: "acme/m-1", Explanation: "json one"},
		2: {Status: pi.StatusCompleted, Model: "acme/m-1", Explanation: "json two"},
	}
	fake.startGate = make(chan struct{})
	fake.startGateAt = 2
	fake.releaseByWorker = map[int]chan struct{}{
		1: make(chan struct{}),
		2: make(chan struct{}),
	}
	fake.completed = make(chan int, 2)

	var code int
	var stdout, stderr string
	done := make(chan struct{})
	go func() {
		defer close(done)
		code, stdout, stderr = runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "one", "--task", "two", "--json"}, "")
	}()
	waitForWorkerCount(t, fake, 2)
	close(fake.releaseByWorker[2])
	waitForWorkerCompleted(t, fake.completed, 2)
	close(fake.releaseByWorker[1])
	waitForWorkerCompleted(t, fake.completed, 1)
	<-done

	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if strings.Count(stderr, "pi-worker: warning:") != 1 {
		t.Fatalf("stderr = %q", stderr)
	}
	output := decodeRunOutput(t, stdout)
	if output.SchemaVersion != 1 || output.Status != "completed" || output.Outcome != contracts.OutcomeCompleted {
		t.Fatalf("output = %#v", output)
	}
	if len(output.Workers) != 2 {
		t.Fatalf("workers = %d, want 2", len(output.Workers))
	}
	if output.Workers[0].Explanation != "json one" {
		t.Fatalf("worker 1 = %#v", output.Workers[0])
	}
	if output.Workers[1].Explanation != "json two" {
		t.Fatalf("worker 2 = %#v", output.Workers[1])
	}
}

func TestRunExitCodes(t *testing.T) {
	tests := []struct {
		name        string
		res         pi.WorkerResult
		want        int
		wantOutcome contracts.Outcome
	}{
		{name: "task failure", res: pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusFailed, Error: "agent failed"}, want: 5, wantOutcome: contracts.OutcomeTaskFailed},
		{name: "readiness", res: pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusUnavailable, Error: "model not available"}, want: 3, wantOutcome: contracts.OutcomeWorkersUnavailable},
		{name: "protocol", res: pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusError, Error: "protocol error"}, want: 9, wantOutcome: contracts.OutcomeInternalError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			installFakeWorker(t, test.res)
			code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "x"}, "")
			if code != test.want {
				t.Fatalf("human exit = %d, want %d", code, test.want)
			}
			if !strings.Contains(stderr, test.res.Error) {
				t.Fatalf("human stderr = %q, want detail %q", stderr, test.res.Error)
			}
			// A failed run still carries its one change-manifest line and
			// the final outcome line: the manifest is measured on every
			// terminal status, and the outcome word names the exit code.
			lines := strings.Split(strings.TrimSpace(stdout), "\n")
			if len(lines) != 2 || !strings.HasPrefix(lines[0], "changes: ") || !strings.HasPrefix(lines[1], "outcome=") {
				t.Fatalf("human stdout = %q, want the changes: line and an outcome= line", stdout)
			}

			_ = installFakeWorker(t, test.res)
			code, stdout, stderr = runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "x", "--json"}, "")
			if code != test.want {
				t.Fatalf("json exit = %d, want %d", code, test.want)
			}
			output := decodeRunOutput(t, stdout)
			if output.SchemaVersion != 1 {
				t.Fatalf("schemaVersion = %d, want 1", output.SchemaVersion)
			}
			if output.Status != "failed" {
				t.Fatalf("status = %q", output.Status)
			}
			if output.Outcome != test.wantOutcome {
				t.Fatalf("outcome = %q, want %q", output.Outcome, test.wantOutcome)
			}
			if len(output.Workers) != 1 || output.Workers[0].Status != test.res.Status || output.Workers[0].Error != test.res.Error {
				t.Fatalf("workers = %#v", output.Workers)
			}
			if !strings.Contains(stderr, test.res.Error) {
				t.Fatalf("json stderr = %q, want detail %q", stderr, test.res.Error)
			}
		})
	}
}

func TestRunExitCodePartialAndLabeledErrors(t *testing.T) {
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	fake.resultsByWorker = map[int]pi.WorkerResult{
		1: {Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "primary done"},
		2: {Model: "acme/m-1", Status: pi.StatusFailed, Error: "agent failed"},
	}

	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "a", "--task", "b"}, "")
	if code != 5 {
		t.Fatalf("human exit = %d, want 5", code)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 3 || lines[0] != "worker 1: primary done" || !strings.HasPrefix(lines[1], "changes: ") || lines[2] != "outcome=partial" {
		t.Fatalf("stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "pi-worker: worker 2: agent failed") {
		t.Fatalf("human stderr = %q", stderr)
	}

	fake = installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	fake.resultsByWorker = map[int]pi.WorkerResult{
		1: {Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "primary done"},
		2: {Model: "acme/m-1", Status: pi.StatusFailed, Error: "agent failed"},
	}
	code, stdout, stderr = runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "a", "--task", "b", "--json"}, "")
	if code != 5 {
		t.Fatalf("json exit = %d, want 5", code)
	}
	if strings.TrimSpace(stdout) == "" {
		t.Fatalf("json stdout is empty")
	}
	output := decodeRunOutput(t, stdout)
	if output.Status != "partial" {
		t.Fatalf("status = %q", output.Status)
	}
	if output.Outcome != contracts.OutcomePartial {
		t.Fatalf("outcome = %q, want %q", output.Outcome, contracts.OutcomePartial)
	}
	if len(output.Workers) != 2 {
		t.Fatalf("workers = %v", output.Workers)
	}
	if !strings.Contains(stderr, "pi-worker: worker 2: agent failed") {
		t.Fatalf("json stderr = %q", stderr)
	}
}

func TestRunAllUnavailableExitCode3(t *testing.T) {
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusUnavailable, Error: "model unavailable"})
	fake.resultsByWorker = map[int]pi.WorkerResult{
		1: {Model: "acme/m-1", Status: pi.StatusUnavailable, Error: "model unavailable"},
		2: {Model: "acme/m-1", Status: pi.StatusUnavailable, Error: "adapter unavailable"},
	}
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "a", "--task", "b", "--json"}, "")
	if code != 3 {
		t.Fatalf("exit = %d, want 3", code)
	}
	output := decodeRunOutput(t, stdout)
	if output.Status != "failed" {
		t.Fatalf("status = %q", output.Status)
	}
	if len(output.Workers) != 2 {
		t.Fatalf("workers = %d, want 2", len(output.Workers))
	}
	for _, w := range output.Workers {
		if w.Status != pi.StatusUnavailable {
			t.Fatalf("worker status = %q", w.Status)
		}
	}
	if !strings.Contains(stderr, "pi-worker: worker 1: model unavailable") || !strings.Contains(stderr, "pi-worker: worker 2: adapter unavailable") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestRunStatusErrorAndUnavailableExitCode9(t *testing.T) {
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	fake.resultsByWorker = map[int]pi.WorkerResult{
		1: {Model: "acme/m-1", Status: pi.StatusError, Error: "protocol error"},
		2: {Model: "acme/m-1", Status: pi.StatusUnavailable, Error: "model unavailable"},
	}
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "a", "--task", "b", "--json"}, "")
	if code != 9 {
		t.Fatalf("exit = %d, want 9", code)
	}
	output := decodeRunOutput(t, stdout)
	if output.Status != "failed" {
		t.Fatalf("status = %q", output.Status)
	}
	if len(output.Workers) != 2 {
		t.Fatalf("workers = %d, want 2", len(output.Workers))
	}
	if output.Workers[0].Status != pi.StatusError {
		t.Fatalf("worker 1 status = %q", output.Workers[0].Status)
	}
	if output.Workers[1].Status != pi.StatusUnavailable {
		t.Fatalf("worker 2 status = %q", output.Workers[1].Status)
	}
	if !strings.Contains(stderr, "pi-worker: worker 1: protocol error") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestRunTimeoutFlag(t *testing.T) {
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted, Explanation: "ok"})
	// Anchor the measurement before the run: the deadline is fixed at
	// 250ms from when the run created it, so measuring against this
	// anchor does not decay while the run completes the way
	// time.Until(deadline) measured afterwards would.
	start := time.Now()
	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "x", "--timeout", "250ms"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	deadline, ok := fake.deadlineForWorker(1)
	if !ok {
		t.Fatalf("worker had no deadline")
	}
	// Distance from the pre-run anchor, not from now: the deadline was
	// created during the run, so it is always at least 250ms past the
	// anchor and at most ~250ms of run-setup overhead past it.
	fromStart := deadline.Sub(start)
	if fromStart < 250*time.Millisecond || fromStart > 500*time.Millisecond {
		t.Fatalf("deadline is %v from run start, want about 250ms", fromStart)
	}
}

func TestRunParentDeadlineExits7FromTimedOutContext(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_ = installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "ok"})
	code, stdout, stderr := runCLIWithContext(t, ctx, []string{"run", "--model", "acme/m-1", "--task", "x", "--json"}, "")
	if code != 7 {
		t.Fatalf("exit = %d, want 7; stderr = %q", code, stderr)
	}
	output := decodeRunOutput(t, stdout)
	if output.Status != "timed-out" {
		t.Fatalf("status = %q", output.Status)
	}
	if output.Outcome != contracts.OutcomeTimeout {
		t.Fatalf("outcome = %q, want %q", output.Outcome, contracts.OutcomeTimeout)
	}
	if len(output.Workers) != 1 || output.Workers[0].Status != pi.StatusTimedOut {
		t.Fatalf("workers = %#v", output.Workers)
	}
	// The context was already done at inspection: the manifest must
	// read as omitted with a stated reason, never as an absent field.
	if output.Changes == nil || output.Changes.Omitted != "context already done" {
		t.Fatalf("changes = %#v, want omitted with %q", output.Changes, "context already done")
	}
}

func TestRunParentCancellationExits8FromCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "ok"})
	code, stdout, stderr := runCLIWithContext(t, ctx, []string{"run", "--model", "acme/m-1", "--task", "x", "--json"}, "")
	if code != 8 {
		t.Fatalf("exit = %d, want 8; stderr = %q", code, stderr)
	}
	output := decodeRunOutput(t, stdout)
	if output.Status != "cancelled" {
		t.Fatalf("status = %q", output.Status)
	}
	if output.Outcome != contracts.OutcomeCancelled {
		t.Fatalf("outcome = %q, want %q", output.Outcome, contracts.OutcomeCancelled)
	}
	if len(output.Workers) != 1 || output.Workers[0].Status != pi.StatusCancelled {
		t.Fatalf("workers = %#v", output.Workers)
	}
	// The context was already done at inspection: the manifest must
	// read as omitted with a stated reason, never as an absent field.
	if output.Changes == nil || output.Changes.Omitted != "context already done" {
		t.Fatalf("changes = %#v, want omitted with %q", output.Changes, "context already done")
	}
}

func TestRunTimedOutContextHumanPrintsOutcomeLineLast(t *testing.T) {
	// The timed-out worker's message goes to stderr; stdout is exactly
	// the change-manifest line followed by the final outcome line, and
	// the outcome word names the timed-out exit.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "ok"})
	code, stdout, stderr := runCLIWithContext(t, ctx, []string{"run", "--model", "acme/m-1", "--task", "x"}, "")
	if code != 7 {
		t.Fatalf("exit = %d, want 7; stderr = %q", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "changes: ") || lines[1] != "outcome=timeout" {
		t.Fatalf("human stdout = %q, want one changes: line then exactly outcome=timeout", stdout)
	}
}

func TestRunCancelledContextHumanPrintsOutcomeLineLast(t *testing.T) {
	// The cancelled worker's message goes to stderr; stdout is exactly
	// the change-manifest line followed by the final outcome line, and
	// the outcome word names the cancelled exit.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "ok"})
	code, stdout, stderr := runCLIWithContext(t, ctx, []string{"run", "--model", "acme/m-1", "--task", "x"}, "")
	if code != 8 {
		t.Fatalf("exit = %d, want 8; stderr = %q", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "changes: ") || lines[1] != "outcome=cancelled" {
		t.Fatalf("human stdout = %q, want one changes: line then exactly outcome=cancelled", stdout)
	}
}

func TestRunUsageShowsDebugAndThinkingFlags(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Main([]string{}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--debug") {
		t.Fatalf("usage does not mention --debug: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "--thinking <level>") {
		t.Fatalf("usage does not mention --thinking: %q", stderr.String())
	}
}

func TestRunDebugPassesSinkAndLogsToStderr(t *testing.T) {
	fake := installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "All done."})
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "fix it", "--debug"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	requireChangesTail(t, stdout, "worker 1: All done.\n")
	request := mustWorkerRequest(t, fake, 1)
	if request.Debug == nil {
		t.Fatalf("debug sink not passed to worker")
	}
	if !strings.Contains(stderr, "[pi-worker +") || !strings.Contains(stderr, "worker=1 phase=starting provider=acme model=m-1") || !strings.Contains(stderr, "worker=1 status=completed total=") {
		t.Fatalf("stderr missing lifecycle logs: %q", stderr)
	}
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		if !strings.HasPrefix(line, "[pi-worker +") {
			t.Fatalf("stderr line without debug prefix: %q", line)
		}
	}
}

func TestRunWithoutDebugKeepsStderrClean(t *testing.T) {
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted, Explanation: "ok"})
	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "x"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if request := mustWorkerRequest(t, fake, 1); request.Debug != nil {
		t.Fatalf("debug sink passed without --debug")
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty without --debug", stderr)
	}
}

func TestRunDebugJSONKeepsStdoutSingleDocument(t *testing.T) {
	fake := installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "JSON answer"})
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--json", "--debug"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
		t.Fatalf("stdout has more than one line: %q", stdout)
	}
	output := decodeRunOutput(t, stdout)
	if output.SchemaVersion != 1 || output.Status != "completed" || output.Workers[0].Explanation != "JSON answer" {
		t.Fatalf("output = %#v", output)
	}
	request := mustWorkerRequest(t, fake, 1)
	if request.Debug == nil {
		t.Fatalf("debug sink not passed with --json --debug")
	}
	if !strings.Contains(stderr, "[pi-worker +") {
		t.Fatalf("stderr missing debug logs: %q", stderr)
	}
}

func TestRunParallelDebugLabelsWorkers1To3(t *testing.T) {
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted})
	fake.resultsByWorker = map[int]pi.WorkerResult{
		1: {Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "done"},
		2: {Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "done"},
		3: {Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "done"},
	}
	fake.startGate = make(chan struct{})
	fake.startGateAt = 3
	fake.releaseByWorker = map[int]chan struct{}{
		1: make(chan struct{}),
		2: make(chan struct{}),
		3: make(chan struct{}),
	}
	fake.completed = make(chan int, 3)

	var code int
	var stdout, stderr string
	done := make(chan struct{})
	go func() {
		defer close(done)
		code, stdout, stderr = runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "a", "--task", "b", "--task", "c", "--debug"}, "")
	}()
	waitForWorkerCount(t, fake, 3)
	close(fake.releaseByWorker[3])
	waitForWorkerCompleted(t, fake.completed, 3)
	close(fake.releaseByWorker[2])
	waitForWorkerCompleted(t, fake.completed, 2)
	close(fake.releaseByWorker[1])
	waitForWorkerCompleted(t, fake.completed, 1)
	<-done

	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	requireChangesTail(t, stdout, "worker 1: done\nworker 2: done\nworker 3: done\n")
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	var debugLines []string
	for _, line := range lines {
		if strings.HasPrefix(line, "pi-worker: warning:") {
			continue
		}
		if !strings.HasPrefix(line, "[pi-worker +") {
			t.Fatalf("stderr line without debug prefix: %q", line)
		}
		if strings.Count(line, "worker=") != 1 {
			t.Fatalf("stderr line has wrong worker labels: %q", line)
		}
		debugLines = append(debugLines, line)
	}
	if len(debugLines) != 6 {
		t.Fatalf("debug lines = %d, want 6: %q", len(debugLines), debugLines)
	}
	for _, worker := range []string{"worker=1 ", "worker=2 ", "worker=3 "} {
		if !strings.Contains(stderr, worker) {
			t.Fatalf("stderr missing %s: %q", worker, stderr)
		}
	}
}

func TestRunAlreadyCancelledContextExits8WithoutLaunchingPi(t *testing.T) {
	// An already-cancelled parent context must flow through runCommand into
	// the worker and surface as status "cancelled" with exit code 8. The
	// host executable must never be launched, so no session directory is
	// created and no process needs reaping.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	installRealFakePiWorker(t)
	setupFakePiScript(t, &script.Script{})
	metaPath := filepath.Join(t.TempDir(), "meta.json")
	t.Setenv("FAKEPI_META", metaPath)

	code, stdout, stderr := runCLIWithContext(t, ctx, []string{"run", "--model", "acme/m-1", "--task", "go", "--json"}, "")
	if code != 8 {
		t.Fatalf("exit = %d, want 8; stderr = %q", code, stderr)
	}
	output := decodeRunOutput(t, stdout)
	if output.Status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", output.Status)
	}
	if output.Outcome != contracts.OutcomeCancelled {
		t.Fatalf("outcome = %q, want %q", output.Outcome, contracts.OutcomeCancelled)
	}
	if _, err := os.Stat(metaPath); !os.IsNotExist(err) {
		t.Fatalf("pi was launched for an already-cancelled run")
	}
}

func TestRunCancellationDuringRunExits8ReapsAndCleansUp(t *testing.T) {
	// Cancelling the parent context mid-run must terminate and reap the
	// child through the worker's normal cleanup (process exit debug line
	// proves cmd.Wait returned), remove the private session directory, and
	// exit 8 with status "cancelled" — never the OS default signal exit.
	scriptConfig := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}},
		},
		"set_model": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"provider":"acme","id":"m-1"}`)}},
		},
		"prompt": {
			{Response: &script.Response{Success: true}},
			{SleepMS: 10000},
		},
	}}
	installRealFakePiWorker(t)
	setupFakePiScript(t, scriptConfig)
	metaPath := filepath.Join(t.TempDir(), "meta.json")
	t.Setenv("FAKEPI_META", metaPath)

	// Cancel only after the child demonstrably received the prompt: the
	// child boot time varies by machine, so a fixed timer would race it.
	// This proves cancellation during a real mid-run, not during startup.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var code int
	var stdout, stderr string
	done := make(chan struct{})
	go func() {
		defer close(done)
		var out bytes.Buffer
		var errBuf bytes.Buffer
		code = mainWithContext(ctx, []string{"run", "--model", "acme/m-1", "--task", "go", "--json", "--debug"}, strings.NewReader(""), &out, &errBuf)
		stdout, stderr = out.String(), errBuf.String()
	}()
	waitForRequestLog(t, os.Getenv("FAKEPI_LOG"), "prompt")
	cancel()
	<-done

	if code != 8 {
		t.Fatalf("exit = %d, want 8; stderr = %q", code, stderr)
	}
	output := decodeRunOutput(t, stdout)
	if output.Status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", output.Status)
	}
	if output.Outcome != contracts.OutcomeCancelled {
		t.Fatalf("outcome = %q, want %q", output.Outcome, contracts.OutcomeCancelled)
	}
	if _, err := os.Stat(sessionDirFromMeta(t, metaPath)); !os.IsNotExist(err) {
		t.Fatalf("session directory left behind after cancellation: %v", err)
	}
	// The child was reaped through normal cleanup: the worker's completion
	// line is logged by the run defer only after the reaping-close defer
	// (child Wait collected, session directory removed), both before the
	// CLI exits.
	if !strings.Contains(stderr, "worker=1 status=cancelled total=") {
		t.Fatalf("stderr missing cancelled completion line: %q", stderr)
	}
}

func TestMainUsageIncludesSkillCommands(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Main([]string{}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(stderr.String(), "pi-worker skill status [--json]") {
		t.Fatalf("usage missing status command: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "pi-worker skill receipt-path [--json]") {
		t.Fatalf("usage missing receipt-path command: %q", stderr.String())
	}
}

func TestMainSupportsSkillStatusCommand(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	writeFileForSkillTest(t, filepath.Join(target, "skill.txt"), "skill")
	writeFileForSkillTest(t, filepath.Join(target, skillinstall.IdentityFile), skillinstall.IdentityContent)
	writeFileForSkillTest(t, filepath.Join(target, "SKILL.md"), "---\nname: pi-worker\n---\n")
	path := filepath.Join(root, "skill-install.json")
	writeReceiptForSkillTest(t, path, skillinstall.Receipt{
		SchemaVersion:    skillinstall.SchemaVersion,
		InstallerVersion: "1",
		SkillsVersion:    skillinstall.PinnedSkillsVersion,
		Outcome:          skillinstall.OutcomeInstalled,
		Targets: []skillinstall.Target{{
			Path: target,
			Kind: "canonical",
			Files: []skillinstall.FileHash{
				{Path: "skill.txt", SHA256: hashString(t, "skill")},
				{Path: skillinstall.IdentityFile, SHA256: hashString(t, skillinstall.IdentityContent)},
				{Path: "SKILL.md", SHA256: hashString(t, "---\nname: pi-worker\n---\n")},
			},
		}},
	})
	installSkillReceiptPath(t, path)

	code, _, stderr := runCLI(t, []string{"skill", "status", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d; stderr = %q", code, stderr)
	}
}

func TestMainCancelsSkillCommandsCleanly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "skill-install.json")
	writeReceiptForSkillTest(t, path, skillinstall.Receipt{
		SchemaVersion:    skillinstall.SchemaVersion,
		InstallerVersion: "1",
		SkillsVersion:    "1",
		Outcome:          skillinstall.OutcomeInstalled,
	})
	installSkillReceiptPath(t, path)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, stdout, _ := runCLIWithContext(t, ctx, []string{"skill", "status", "--json"}, "")
	if code != 8 || stdout != "" {
		t.Fatalf("exit = %d, stdout = %q", code, stdout)
	}
}

func TestPrintGitChangeUnbornToCommittedHeadWarning(t *testing.T) {
	// A run that starts on an unborn branch has no HEAD. The warning must
	// render the empty hash as (none) instead of a blank, so an
	// unborn-to-committed run reads "HEAD (none) -> 8b970ca" and not
	// "HEAD  -> 8b970ca" with a doubled space.
	var stderr bytes.Buffer
	printGitChange(&run.GitChange{
		Before: run.GitState{Head: ""},
		After:  run.GitState{Head: "8b970ca6db30a27c713aca1f1ee2974c31cfde3d"},
	}, &stderr)
	want := "pi-worker: warning: the run changed git state: HEAD (none) -> 8b970ca\n"
	if got := stderr.String(); got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}
}

func TestPrintGitChangeStashRemovalWarning(t *testing.T) {
	// A balanced drop-and-push leaves the stash count unchanged; the
	// warning must come from the entry diff, not the count, and must
	// render the entry at the seven-character abbreviation, not the full
	// sha.
	var stderr bytes.Buffer
	printGitChange(&run.GitChange{
		Before: run.GitState{Head: "8b970ca6db30a27c713aca1f1ee2974c31cfde3d", Branch: "main", Stashes: 2},
		After:  run.GitState{Head: "8b970ca6db30a27c713aca1f1ee2974c31cfde3d", Branch: "main", Stashes: 2},
		Stash: &run.GitStashChange{
			Removed: []string{"e3a12fa54f8deacc23254771d8235abc1b5d9497 WIP on main: 4ae275a init"},
		},
	}, &stderr)
	want := "pi-worker: warning: the run changed git state: stash removed: e3a12fa WIP on main: 4ae275a init\n"
	if got := stderr.String(); got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}
}

func TestPrintGitChangeStashListCapsAtThreeEntries(t *testing.T) {
	var stderr bytes.Buffer
	printGitChange(&run.GitChange{
		Before: run.GitState{Head: "same", Branch: "main", Stashes: 5},
		After:  run.GitState{Head: "same", Branch: "main", Stashes: 0},
		Stash: &run.GitStashChange{
			Removed: []string{
				"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa first",
				"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb second",
				"cccccccccccccccccccccccccccccccccccccccc third",
				"dddddddddddddddddddddddddddddddddddddddd fourth",
				"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee fifth",
			},
		},
	}, &stderr)
	want := "pi-worker: warning: the run changed git state: stash removed: aaaaaaa first; bbbbbbb second; ccccccc third; and 2 more\n"
	if got := stderr.String(); got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}
}

// TestPrintChanges* build the run.Changes struct directly and pin the
// exact human rendering: printChanges renders, it does not sort, so every
// fixture is already in the order the manifest produces (most churn
// first, then path).

func TestPrintChangesOmittedManifest(t *testing.T) {
	// An omitted manifest prints the reason line alone, never a path
	// list.
	var stdout bytes.Buffer
	printChanges(&run.Changes{Omitted: "measurement failed"}, &stdout)
	want := "changes: omitted: measurement failed\n"
	if got := stdout.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestPrintChangesZeroFiles(t *testing.T) {
	// A measured run that changed nothing prints the zero line alone.
	var stdout bytes.Buffer
	printChanges(&run.Changes{TotalFiles: 0}, &stdout)
	want := "changes: 0 files, +0/-0\n"
	if got := stdout.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestPrintChangesOneFile(t *testing.T) {
	// A single changed path reads "1 file", not "1 files".
	var stdout bytes.Buffer
	printChanges(&run.Changes{
		TotalFiles: 1,
		Files: []run.FileChange{
			{Path: "src/a.go", Status: "modified", Added: 3, Deleted: 1},
		},
	}, &stdout)
	want := "changes: 1 file, +3/-1\n  src/a.go  +3/-1\n"
	if got := stdout.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestPrintChangesSeveralFiles(t *testing.T) {
	// The count line sums the entries' added and deleted lines; paths
	// print in the manifest's order, most churn first, indented two
	// spaces.
	var stdout bytes.Buffer
	printChanges(&run.Changes{
		TotalFiles: 3,
		Files: []run.FileChange{
			{Path: "docs/guide.md", Status: "modified", Added: 12, Deleted: 4},
			{Path: "src/main.go", Status: "modified", Added: 5, Deleted: 2},
			{Path: "README.md", Status: "added", Added: 8, Deleted: 0},
		},
	}, &stdout)
	want := "changes: 3 files, +25/-6\n  docs/guide.md  +12/-4\n  src/main.go  +5/-2\n  README.md  +8/-0\n"
	if got := stdout.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestPrintChangesBinaryFile(t *testing.T) {
	// A binary entry renders as "binary" on its path line, never as
	// "+0/-0", and still contributes zero to the summed counts.
	var stdout bytes.Buffer
	printChanges(&run.Changes{
		TotalFiles: 1,
		Files: []run.FileChange{
			{Path: "assets/logo.png", Status: "added", Binary: true},
		},
	}, &stdout)
	want := "changes: 1 file, +0/-0\n  assets/logo.png  binary\n"
	if got := stdout.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestPrintChangesMoreThanFiveFiles(t *testing.T) {
	// Exactly five paths print, most churn first; the rest collapse into
	// the trailing line, one per entry the five-line limit dropped.
	var stdout bytes.Buffer
	printChanges(&run.Changes{
		TotalFiles: 7,
		Files: []run.FileChange{
			{Path: "a1.go", Status: "modified", Added: 9},
			{Path: "a2.go", Status: "modified", Added: 8},
			{Path: "a3.go", Status: "modified", Added: 7},
			{Path: "a4.go", Status: "modified", Added: 6},
			{Path: "a5.go", Status: "modified", Added: 5},
			{Path: "a6.go", Status: "modified", Added: 4},
			{Path: "a7.go", Status: "modified", Added: 3},
		},
	}, &stdout)
	want := "changes: 7 files, +42/-0\n  a1.go  +9/-0\n  a2.go  +8/-0\n  a3.go  +7/-0\n  a4.go  +6/-0\n  a5.go  +5/-0\n  and 2 more\n"
	if got := stdout.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestPrintChangesTrailingCountIsRelativeToTotalFiles(t *testing.T) {
	// A truncated manifest carries TotalFiles larger than len(Files):
	// the entry cap dropped paths beyond it. The trailing line counts
	// from TotalFiles, so it reports the paths the cap dropped as well
	// as the ones the five-line limit dropped: with 120 changed paths,
	// six of them in the list, five printed, the human is told 115
	// paths are not on screen.
	var stdout bytes.Buffer
	printChanges(&run.Changes{
		TotalFiles: 120,
		Truncated:  true,
		Files: []run.FileChange{
			{Path: "c1.go", Status: "modified", Added: 1},
			{Path: "c2.go", Status: "modified", Added: 1},
			{Path: "c3.go", Status: "modified", Added: 1},
			{Path: "c4.go", Status: "modified", Added: 1},
			{Path: "c5.go", Status: "modified", Added: 1},
			{Path: "c6.go", Status: "modified", Added: 1},
		},
	}, &stdout)
	want := "changes: 120 files, +6/-0\n  c1.go  +1/-0\n  c2.go  +1/-0\n  c3.go  +1/-0\n  c4.go  +1/-0\n  c5.go  +1/-0\n  and 115 more\n"
	if got := stdout.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestPrintChangesDirtyBeforeClause(t *testing.T) {
	// An entry that was already dirty before the run names the fact on
	// the header line: its counts are measured against the last commit
	// and include the caller's own uncommitted work, so the summed
	// +added/-deleted would otherwise read inflated. One entry reads
	// "1 already modified before the run"; the phrase is not pluralised,
	// only the number changes.
	var stdout bytes.Buffer
	printChanges(&run.Changes{
		TotalFiles: 2,
		Files: []run.FileChange{
			{Path: "src/a.go", Status: "modified", Added: 3, Deleted: 1, DirtyBefore: true},
			{Path: "README.md", Status: "added", Added: 8},
		},
	}, &stdout)
	want := "changes: 2 files, +11/-1 (1 already modified before the run)\n  src/a.go  +3/-1\n  README.md  +8/-0\n"
	if got := stdout.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}

	var many bytes.Buffer
	printChanges(&run.Changes{
		TotalFiles: 2,
		Files: []run.FileChange{
			{Path: "src/a.go", Status: "modified", Added: 3, Deleted: 1, DirtyBefore: true},
			{Path: "src/b.go", Status: "modified", Added: 2, DirtyBefore: true},
		},
	}, &many)
	wantMany := "changes: 2 files, +5/-1 (2 already modified before the run)\n  src/a.go  +3/-1\n  src/b.go  +2/-0\n"
	if got := many.String(); got != wantMany {
		t.Fatalf("output = %q, want %q", got, wantMany)
	}
}

func TestPrintChangesNoFinalNewlineClause(t *testing.T) {
	// An entry whose last byte is not a newline names the count on the
	// header line, in its own parenthetical and only when the count is
	// above zero: the per-file listing lines below stay unchanged. The
	// field is a measurement, never a verdict, so the clause claims no
	// fault — and when both parentheticals apply they print separated by
	// a space, the dirty-before one first.
	var stdout bytes.Buffer
	printChanges(&run.Changes{
		TotalFiles: 2,
		Files: []run.FileChange{
			{Path: "src/a.go", Status: "modified", Added: 3, Deleted: 1, NoFinalNewline: true},
			{Path: "README.md", Status: "added", Added: 8},
		},
	}, &stdout)
	want := "changes: 2 files, +11/-1 (1 without a final newline)\n  src/a.go  +3/-1\n  README.md  +8/-0\n"
	if got := stdout.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}

	var both bytes.Buffer
	printChanges(&run.Changes{
		TotalFiles: 2,
		Files: []run.FileChange{
			{Path: "a.go", Status: "modified", Added: 1, DirtyBefore: true, NoFinalNewline: true},
			{Path: "b.go", Status: "added", Added: 1, NoFinalNewline: true},
		},
	}, &both)
	wantBoth := "changes: 2 files, +2/-0 (1 already modified before the run) (2 without a final newline)\n  a.go  +1/-0\n  b.go  +1/-0\n"
	if got := both.String(); got != wantBoth {
		t.Fatalf("output = %q, want %q", got, wantBoth)
	}
}

// TestPrintWrites* build the run.WriteCheck struct directly and pin the
// exact human rendering: printWrites renders, it does not sort, so every
// fixture is already in the order the check produces (sorted by path).

func TestPrintWritesNilCheckPrintsNothing(t *testing.T) {
	// A nil check means the caller never declared; there is nothing to
	// report.
	var stdout bytes.Buffer
	printWrites(nil, &stdout)
	if got := stdout.String(); got != "" {
		t.Fatalf("output = %q, want empty", got)
	}
}

func TestPrintWritesCleanVerdict(t *testing.T) {
	// A clean verdict prints one short line on stdout, the whole point
	// of the field: the caller must see that the check ran and passed,
	// not merely that nothing was said.
	var stdout bytes.Buffer
	printWrites(&run.WriteCheck{UndeclaredCount: 0}, &stdout)
	want := "writes: ok\n"
	if got := stdout.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestPrintWritesSkippedPartialDeclaration(t *testing.T) {
	var stdout bytes.Buffer
	printWrites(&run.WriteCheck{Skipped: "not all tasks declared writes"}, &stdout)
	want := "writes: skipped: not all tasks declared writes\n"
	if got := stdout.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestPrintWritesSkippedManifestUnavailable(t *testing.T) {
	var stdout bytes.Buffer
	printWrites(&run.WriteCheck{Skipped: "change manifest unavailable"}, &stdout)
	want := "writes: skipped: change manifest unavailable\n"
	if got := stdout.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestPrintWritesOneUndeclaredPath(t *testing.T) {
	// The violation goes to stderr: it is a failure and it is what exit
	// 4 refers to. The count line names the count, singular for one
	// path, and the path follows indented two spaces.
	var stderr bytes.Buffer
	printWrites(&run.WriteCheck{
		Undeclared:      []string{"src/stray.txt"},
		UndeclaredCount: 1,
	}, &stderr)
	want := "pi-worker: write check failed: 1 undeclared path\n  src/stray.txt\n"
	if got := stderr.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestPrintWritesSeveralUndeclaredPaths(t *testing.T) {
	// Paths print in the check's order, sorted by path, one per line
	// indented two spaces.
	var stderr bytes.Buffer
	printWrites(&run.WriteCheck{
		Undeclared:      []string{"docs/leak.md", "go.mod.bak", "src/stray.txt"},
		UndeclaredCount: 3,
	}, &stderr)
	want := "pi-worker: write check failed: 3 undeclared paths\n" +
		"  docs/leak.md\n  go.mod.bak\n  src/stray.txt\n"
	if got := stderr.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestPrintWritesMoreThanFiveUndeclaredPaths(t *testing.T) {
	// Exactly five paths print; the rest collapse into the trailing
	// line, one per path the five-line limit dropped.
	var stderr bytes.Buffer
	undeclared := []string{"a1.txt", "a2.txt", "a3.txt", "a4.txt", "a5.txt", "a6.txt", "a7.txt"}
	printWrites(&run.WriteCheck{Undeclared: undeclared, UndeclaredCount: 7}, &stderr)
	want := "pi-worker: write check failed: 7 undeclared paths\n" +
		"  a1.txt\n  a2.txt\n  a3.txt\n  a4.txt\n  a5.txt\n  and 2 more\n"
	if got := stderr.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestPrintWritesTrailingCountIsRelativeToUndeclaredCount(t *testing.T) {
	// A truncated check carries UndeclaredCount larger than
	// len(Undeclared): the entry cap dropped paths beyond it. The
	// trailing line counts from UndeclaredCount, so it reports the
	// paths the cap dropped as well as the ones the five-line limit
	// dropped: with 120 undeclared paths, six of them in the list, five
	// printed, the human is told 115 paths are not on screen.
	var stderr bytes.Buffer
	printWrites(&run.WriteCheck{
		Undeclared:      []string{"c1.txt", "c2.txt", "c3.txt", "c4.txt", "c5.txt", "c6.txt"},
		UndeclaredCount: 120,
		Truncated:       true,
	}, &stderr)
	want := "pi-worker: write check failed: 120 undeclared paths\n" +
		"  c1.txt\n  c2.txt\n  c3.txt\n  c4.txt\n  c5.txt\n  and 115 more\n"
	if got := stderr.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestRunExitCodePolicyOnUndeclaredWrites(t *testing.T) {
	// A completed run whose write check found at least one undeclared
	// path exits 4. A skipped check never exits 4: a skip means the
	// question could not be answered, and answering "violation" would
	// be a lie. A clean verdict never exits 4 either, and a run with no
	// declaration has no check at all.
	tests := []struct {
		name        string
		result      run.Result
		want        int
		wantOutcome contracts.Outcome
	}{
		{name: "no declaration", result: run.Result{Status: contracts.RunCompleted}, want: 0, wantOutcome: contracts.OutcomeCompleted},
		{name: "clean verdict", result: run.Result{Status: contracts.RunCompleted, Writes: &run.WriteCheck{UndeclaredCount: 0}}, want: 0, wantOutcome: contracts.OutcomeCompleted},
		{name: "skipped partial declaration", result: run.Result{Status: contracts.RunCompleted, Writes: &run.WriteCheck{Skipped: "not all tasks declared writes"}}, want: 0, wantOutcome: contracts.OutcomeCompleted},
		{name: "skipped manifest unavailable", result: run.Result{Status: contracts.RunCompleted, Writes: &run.WriteCheck{Skipped: "change manifest unavailable"}}, want: 0, wantOutcome: contracts.OutcomeCompleted},
		{name: "undeclared paths", result: run.Result{Status: contracts.RunCompleted, Writes: &run.WriteCheck{Undeclared: []string{"stray.txt"}, UndeclaredCount: 1}}, want: 4, wantOutcome: contracts.OutcomeUndeclaredWrites},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotOutcome, got := runOutcome(test.result)
			if got != test.want {
				t.Fatalf("exit = %d, want %d", got, test.want)
			}
			if gotOutcome != test.wantOutcome {
				t.Fatalf("outcome = %q, want %q", gotOutcome, test.wantOutcome)
			}
		})
	}
}

func TestRunExitCodePrecedence(t *testing.T) {
	// Run-outcome codes win over both checks: a timed-out run that also
	// wrote outside its declaration exits 7, and a cancelled one exits
	// 8. A failed, partial, or all-unavailable run is answered by its
	// own outcome -- 5, 9, or 3 -- never by the check that runs on
	// every terminal status. Among completed runs, policy outranks
	// verification: a run that wrote outside its declared scope has
	// breached the contract the caller relied on to bound it, and
	// whether its tests pass is secondary information the result
	// document carries either way. A completed run with a failing
	// verification and no policy violation still exits 6.
	violation := &run.WriteCheck{Undeclared: []string{"stray.txt"}, UndeclaredCount: 1}
	failingVerification := &run.Verification{Argv: []string{"go", "test"}, ExitCode: 3}
	tests := []struct {
		name        string
		result      run.Result
		want        int
		wantOutcome contracts.Outcome
	}{
		{name: "timed out with violation", result: run.Result{Status: contracts.RunTimedOut, Writes: violation}, want: 7, wantOutcome: contracts.OutcomeTimeout},
		{name: "cancelled with violation", result: run.Result{Status: contracts.RunCancelled, Writes: violation}, want: 8, wantOutcome: contracts.OutcomeCancelled},
		{name: "failed run with violation", result: run.Result{Status: contracts.RunFailed, Workers: []pi.WorkerResult{{Status: pi.StatusFailed}}, Writes: violation}, want: 5, wantOutcome: contracts.OutcomeTaskFailed},
		{name: "internal error with violation", result: run.Result{Status: contracts.RunFailed, Workers: []pi.WorkerResult{{Status: pi.StatusError}}, Writes: violation}, want: 9, wantOutcome: contracts.OutcomeInternalError},
		{name: "partial run with violation", result: run.Result{Status: contracts.RunPartial, Workers: []pi.WorkerResult{{Status: pi.StatusCompleted}}, Writes: violation}, want: 5, wantOutcome: contracts.OutcomePartial},
		{name: "all-unavailable run with violation", result: run.Result{Status: contracts.RunFailed, Workers: []pi.WorkerResult{{Status: pi.StatusUnavailable}}, Writes: violation}, want: 3, wantOutcome: contracts.OutcomeWorkersUnavailable},
		{name: "violation and failing verification", result: run.Result{Status: contracts.RunCompleted, Writes: violation, Verification: failingVerification}, want: 4, wantOutcome: contracts.OutcomeUndeclaredWrites},
		{name: "failing verification alone", result: run.Result{Status: contracts.RunCompleted, Verification: failingVerification}, want: 6, wantOutcome: contracts.OutcomeVerificationFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotOutcome, got := runOutcome(test.result)
			if got != test.want {
				t.Fatalf("exit = %d, want %d", got, test.want)
			}
			if gotOutcome != test.wantOutcome {
				t.Fatalf("outcome = %q, want %q", gotOutcome, test.wantOutcome)
			}
		})
	}
}

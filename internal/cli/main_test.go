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
	runVersionProbe = func(context.Context) (string, error) { return "0.84.1", nil }
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
	Workers       []pi.WorkerResult `json:"workers"`
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

func TestRunSuccessHuman(t *testing.T) {
	fake := installFakeWorker(t, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "All done."})
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "fix the bug"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if stdout != "worker 1: All done.\n" {
		t.Fatalf("stdout = %q", stdout)
	}
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
	remaining := time.Until(deadline)
	if remaining < 29*time.Minute || remaining > 31*time.Minute {
		t.Fatalf("default deadline is %v away, want about 30m", remaining)
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
	if stdout != "worker 1 [model=acme/m-1 thinking=max]: All done.\n" {
		t.Fatalf("stdout = %q", stdout)
	}
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
	logPath := installProcessVersionProbe(t, "0.84.1\n", "", 0)
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
	const wantWarning = "pi-worker: warning: Pi version 0.99.0 is unverified; verified version is 0.84.1; continuing\n"
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
	if stdout != "worker 1: ok\n" {
		t.Fatalf("stdout = %q", stdout)
	}
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
	if stdout != "worker 1: ok\n" {
		t.Fatalf("stdout = %q", stdout)
	}
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
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("stdout lines = %d, want 3: %q", len(lines), stdout)
	}
	for i, want := range []string{"worker 1: first done", "worker 2: second done", "worker 3: third done"} {
		if lines[i] != want {
			t.Fatalf("stdout line %d = %q, want %q", i+1, lines[i], want)
		}
	}
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
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout lines = %d, want 2: %q", len(lines), stdout)
	}
	if lines[0] != "worker 1: first file done" || lines[1] != "worker 2: second file done" {
		t.Fatalf("stdout = %q", stdout)
	}
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
	if output.SchemaVersion != 1 || output.Status != "completed" {
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
		name string
		res  pi.WorkerResult
		want int
	}{
		{name: "task failure", res: pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusFailed, Error: "agent failed"}, want: 5},
		{name: "readiness", res: pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusUnavailable, Error: "model not available"}, want: 3},
		{name: "protocol", res: pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusError, Error: "protocol error"}, want: 9},
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
			if stdout != "" {
				t.Fatalf("human stdout = %q", stdout)
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
	if len(lines) != 1 || lines[0] != "worker 1: primary done" {
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
	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "x", "--timeout", "250ms"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	deadline, ok := fake.deadlineForWorker(1)
	if !ok {
		t.Fatalf("worker had no deadline")
	}
	remaining := time.Until(deadline)
	if remaining < 0 || remaining > 500*time.Millisecond {
		t.Fatalf("deadline is %v away, want about 250ms", remaining)
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
	if len(output.Workers) != 1 || output.Workers[0].Status != pi.StatusTimedOut {
		t.Fatalf("workers = %#v", output.Workers)
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
	if len(output.Workers) != 1 || output.Workers[0].Status != pi.StatusCancelled {
		t.Fatalf("workers = %#v", output.Workers)
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
	if stdout != "worker 1: All done.\n" {
		t.Fatalf("stdout = %q", stdout)
	}
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
	if stdout != "worker 1: done\nworker 2: done\nworker 3: done\n" {
		t.Fatalf("stdout = %q", stdout)
	}
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

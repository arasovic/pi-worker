package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/background"
	"github.com/arasovic/pi-worker/internal/buildinfo"
	"github.com/arasovic/pi-worker/internal/config"
	"github.com/arasovic/pi-worker/internal/contracts"
	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/piversion"
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
	// a deliberately minimal environment. The verification-command helper
	// runs in the run's workspace, where the fake-Pi package cannot build.
	if os.Getenv(interruptHelperEnv) == "1" || os.Getenv("PI_WORKER_CLI_VERIFY_HELPER") == "1" {
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
	runVersionProbe = func(context.Context) (string, error) { return piversion.VerifiedVersion, nil }
	// Point the runlogDir seam at a parent temporary directory so no test
	// that reaches the recorder writes into the user's real records
	// directory. Each invocation of the seam now creates and returns a
	// fresh child inside that parent, preventing run-ID collisions
	// between test invocations in the same process and second. Tests that
	// intentionally need shared history already override the seam
	// themselves. The redirect is restored on the same path where the
	// fakepi build directory is removed below.
	runlogParent, err := os.MkdirTemp("", "pi-worker-cli-runlog-parent-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create runlog parent directory: %v\n", err)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	originalRunlogDir := runlogDir
	runlogDir = func() (string, error) {
		return os.MkdirTemp(runlogParent, "pi-worker-cli-runlog-*")
	}
	// Point the userConfigPath seam at one temporary directory for the
	// whole package test run too, so no test that reaches the run
	// configuration reads the user's real config.json: the file is left
	// absent here, so the loader reports the not-exist error and callers
	// fall back to config.Empty() defaults, and any foreground admission
	// state derived beside the config file stays under the system
	// temporary directory with the fake-Pi and runlog resources. Tests
	// that need a real on-disk config install their own path over this
	// redirect. The seam is restored on the same path where the other
	// temporary directories are removed below.
	configDir, err := os.MkdirTemp("", "pi-worker-cli-config-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create config directory: %v\n", err)
		os.RemoveAll(dir)
		os.RemoveAll(runlogParent)
		os.Exit(1)
	}
	originalUserConfigPath := userConfigPath
	userConfigPath = func() (string, error) { return filepath.Join(configDir, "config.json"), nil }
	code := m.Run()
	runVersionProbe = originalRunVersionProbe
	runlogDir = originalRunlogDir
	userConfigPath = originalUserConfigPath
	os.RemoveAll(dir)
	os.RemoveAll(runlogParent)
	os.RemoveAll(configDir)
	os.Exit(code)
}

func runCLI(t *testing.T, args []string, stdin string) (int, string, string) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	ran := separateRunSeconds(args)
	code := Main(args, strings.NewReader(stdin), &stdout, &stderr)
	ran(code)
	return code, stdout.String(), stderr.String()
}

// lastRunSecond is the second the latest in-process `run` command that may
// have started a run returned in. A run identity is the start second plus
// the starting process's pid, and every run started in-process shares this
// test process's pid, so two runs started in one second against one state
// root would claim the same identity. A helper that gives a test a fresh
// state root clears it, since no earlier run can collide there.
var lastRunSecond time.Time

// separateRunSeconds keeps in-process runs in distinct seconds: before a
// `run` command it waits until the clock has left the second the previous
// run returned in — that run was accepted no later than that — and the
// returned func records the second this command returned in, unless it was
// refused as a usage error and so started nothing.
func separateRunSeconds(args []string) func(code int) {
	if len(args) == 0 || args[0] != "run" {
		return func(int) {}
	}
	if now := time.Now().Truncate(time.Second); !now.After(lastRunSecond) {
		time.Sleep(time.Until(lastRunSecond.Add(time.Second)))
	}
	return func(code int) {
		if code != 2 {
			lastRunSecond = time.Now().Truncate(time.Second)
		}
	}
}

// runCLIWithContext drives the private Main seam with an explicit parent
// context so cancellation tests are deterministic and never send a real
// signal to the test process.
func runCLIWithContext(t *testing.T, ctx context.Context, args []string, stdin string) (int, string, string) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	ran := separateRunSeconds(args)
	code := mainWithContext(ctx, args, strings.NewReader(stdin), &stdout, &stderr)
	ran(code)
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

// useFakePi points every run this test starts at the fakepi test double
// answering from scriptConfig, through the supervisor the built binary
// starts. The background state stays under this test's own directory, and
// the admission root and limit the run resolved reach the Manager unchanged,
// as they do in production. The returned Manager reads that state.
func useFakePi(t *testing.T, scriptConfig *script.Script) *background.Manager {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("fake-Pi runs need a platform that can host a role process")
	}
	roleBin := piWorkerBinForBackground(t)
	setupFakePiScript(t, scriptConfig)

	root := t.TempDir()
	lastRunSecond = time.Time{}
	reader, err := background.NewManager(root, t.TempDir(), 1)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	originalManager, originalPi, originalRole := newBackgroundManager, backgroundPiExecutable, backgroundRoleExecutable
	newBackgroundManager = func(admissionRoot string, maxModelWorkers int) (*background.Manager, error) {
		return background.NewManager(root, admissionRoot, maxModelWorkers)
	}
	backgroundPiExecutable = fakePiBin
	backgroundRoleExecutable = roleBin
	t.Cleanup(func() {
		newBackgroundManager, backgroundPiExecutable, backgroundRoleExecutable = originalManager, originalPi, originalRole
	})
	return reader
}

// runLinePattern matches the one stderr line `run` prints once its run is
// accepted.
var runLinePattern = regexp.MustCompile(`(?m)^pi-worker: run ([0-9]{8}T[0-9]{6}Z-[0-9]+)\n`)

// withoutRunLine returns stderr with the accepted run's line removed, so a
// test can compare the rest of stderr exactly. It fails the test unless that
// line appears exactly once.
func withoutRunLine(t *testing.T, stderr string) string {
	t.Helper()
	if n := len(runLinePattern.FindAllString(stderr, -1)); n != 1 {
		t.Fatalf("stderr = %q, want exactly one run line, got %d", stderr, n)
	}
	return runLinePattern.ReplaceAllString(stderr, "")
}

// runIDFromRunLine returns the identity the accepted run's stderr line
// names.
func runIDFromRunLine(t *testing.T, stderr string) string {
	t.Helper()
	match := runLinePattern.FindStringSubmatch(stderr)
	if match == nil {
		t.Fatalf("stderr = %q, want a run line", stderr)
	}
	return match[1]
}

// fakePiDoneLines is the human summary of workers 1 to n that each answered
// "done" through backgroundHappyScript: the fake Pi reports the model and
// its default thinking level, so each line carries both.
func fakePiDoneLines(n int) string {
	var lines strings.Builder
	for id := 1; id <= n; id++ {
		fmt.Fprintf(&lines, "worker %d [model=acme/m-1 thinking=medium]: done\n", id)
	}
	return lines.String()
}

// heldHappyScript is backgroundHappyScript with the prompt held for holdMS
// after it is accepted, so the worker stays in flight that long.
func heldHappyScript(finalText string, holdMS int) *script.Script {
	s := backgroundHappyScript(finalText)
	prompt := s.Triggers["prompt"]
	held := append([]script.Step{prompt[0], {SleepMS: holdMS}}, prompt[1:]...)
	s.Triggers["prompt"] = held
	return s
}

// fakePiRequestOrder reads the fake Pi request log every worker of a run
// appended to and returns its prompt and answer requests in order, P for a
// prompt and A for a request for the answer. Concurrent appends may split a
// line from its newline, so the log is scanned for the request types rather
// than split into lines.
func fakePiRequestOrder(t *testing.T, logPath string) string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake Pi log: %v", err)
	}
	pattern := regexp.MustCompile(`"type":"(prompt|get_last_assistant_text)"`)
	var order strings.Builder
	for _, match := range pattern.FindAllStringSubmatch(string(data), -1) {
		if match[1] == "prompt" {
			order.WriteByte('P')
		} else {
			order.WriteByte('A')
		}
	}
	return order.String()
}

// mustResolveRun resolves a run command line — args without the leading
// "run" — the way `run` does before anything starts, and fails the test on
// a rejection.
func mustResolveRun(t *testing.T, args []string, stdin string) (runOptions, []run.Task) {
	t.Helper()
	opts, tasks, err := resolveRunInput(args, strings.NewReader(stdin))
	if err != nil {
		t.Fatalf("resolveRunInput(%q): %v", args, err)
	}
	return opts, tasks
}

// renderRun prints result the way `run` prints a finished run: it assigns
// the outcome and returns the exit code with what reached each stream.
func renderRun(t *testing.T, result run.Result, jsonOutput bool) (int, string, string) {
	t.Helper()
	outcome, code := runOutcome(result)
	result.Outcome = outcome
	var stdout, stderr bytes.Buffer
	if err := printRunDocument(result, jsonOutput, &stdout, &stderr); err != nil {
		t.Fatalf("printRunDocument: %v", err)
	}
	return code, stdout.String(), stderr.String()
}

// finishedRun is the result the controller returns for workers that ended
// with the aggregate status given, measured on a clean workspace.
func finishedRun(status contracts.RunStatus, workers ...pi.WorkerResult) run.Result {
	return run.Result{SchemaVersion: contracts.SchemaVersion, Status: status, Workers: workers, Changes: &run.Changes{}}
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

// fakePiCwd reads the fakepi meta file and returns the directory the fake
// Pi ran in, symlinks resolved.
func fakePiCwd(t *testing.T, metaPath string) string {
	t.Helper()
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read fakepi meta: %v", err)
	}
	var meta struct {
		Cwd string `json:"cwd"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("decode fakepi meta: %v", err)
	}
	return resolvedDir(t, meta.Cwd)
}

// resolvedDir returns dir with symlinks resolved, so a temporary directory
// compares equal however a process spelled it.
func resolvedDir(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve %s: %v", dir, err)
	}
	return resolved
}

// workerExecutionTimeout returns the executionTimeout field of worker index
// in the run --json document on stdout.
func workerExecutionTimeout(t *testing.T, stdout string, index int) string {
	t.Helper()
	var document struct {
		Workers []struct {
			ExecutionTimeout string `json:"executionTimeout"`
		} `json:"workers"`
	}
	if err := json.Unmarshal([]byte(stdout), &document); err != nil {
		t.Fatalf("decode json stdout: %v (%q)", err, stdout)
	}
	if index >= len(document.Workers) {
		t.Fatalf("workers = %d, want worker %d", len(document.Workers), index+1)
	}
	return document.Workers[index].ExecutionTimeout
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
		{name: "model without value", args: []string{"run", "--model"}, stdin: ""},
		{name: "model without provider", args: []string{"run", "--model", "/m-1"}, stdin: ""},
		{name: "model without id", args: []string{"run", "--model", "acme/"}, stdin: ""},
		{name: "model without slash", args: []string{"run", "--model", "acme"}, stdin: ""},
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
		})
	}
}

// TestRunMissingModelAnswersWithRemedyNotUsage pins the one run
// rejection that is not an argv-shape mistake: the argument shape was
// legal and only the machine state was incomplete. It must answer with
// the diagnosis and the two remedy lines naming both halves of the fix,
// and must not reprint the synopsis.
func TestRunMissingModelAnswersWithRemedyNotUsage(t *testing.T) {
	installConfigPath(t, filepath.Join(t.TempDir(), "config.json"))

	code, stdout, stderr := runCLI(t, []string{"run"}, "do it")
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %q", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if strings.Contains(stderr, "usage:") {
		t.Fatalf("stderr reprints the synopsis: %q", stderr)
	}
	const diagnosis = "pi-worker: missing required flag --model and no configured default model"
	const remedyDefault = "pass --model <provider/model> on this run, or set a default once with: pi-worker config set default-model <provider/model>"
	const remedySelectors = "pi-worker models lists the exact selectors both accept"
	for _, want := range []string{diagnosis, remedyDefault, remedySelectors} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want line %q", stderr, want)
		}
	}
	want := diagnosis + "\n" + remedyDefault + "\n" + remedySelectors + "\n"
	if stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
}

// TestRunRejectsNonUTF8TaskFile requires a task file whose bytes are not
// valid UTF-8 to be refused as a usage error on the foreground path,
// before anything starts: the same bad input the background start
// request encoder used to reject as an internal failure must be caught
// where the rest of the command line is resolved.
func TestRunRejectsNonUTF8TaskFile(t *testing.T) {
	installConfigPath(t, filepath.Join(t.TempDir(), "config.json"))
	taskFile := filepath.Join(t.TempDir(), "task.bin")
	if err := os.WriteFile(taskFile, []byte("\xff\xfe bad"), 0o600); err != nil {
		t.Fatalf("write task file: %v", err)
	}
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task-file", taskFile}, "")
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %q", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, taskFile) {
		t.Fatalf("stderr = %q, want it to name the task file %q", stderr, taskFile)
	}
	if !strings.Contains(stderr, "not valid UTF-8") {
		t.Fatalf("stderr = %q, want the encoding rejection", stderr)
	}
}

// TestRunBackgroundRejectsNonUTF8TaskFile pins the defect: the same task
// file exits 9 with --background when the check only lives in the start
// request encoder. With the check in resolveTasks the rejection happens
// before the background command runs, so no supervisor seam is needed
// and stdout stays empty.
func TestRunBackgroundRejectsNonUTF8TaskFile(t *testing.T) {
	installConfigPath(t, filepath.Join(t.TempDir(), "config.json"))
	taskFile := filepath.Join(t.TempDir(), "task.bin")
	if err := os.WriteFile(taskFile, []byte("\xff\xfe bad"), 0o600); err != nil {
		t.Fatalf("write task file: %v", err)
	}
	code, stdout, stderr := runCLI(t, []string{"run", "--background", "--model", "acme/m-1", "--task-file", taskFile}, "")
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %q", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, taskFile) {
		t.Fatalf("stderr = %q, want it to name the task file %q", stderr, taskFile)
	}
	if !strings.Contains(stderr, "not valid UTF-8") {
		t.Fatalf("stderr = %q, want the encoding rejection", stderr)
	}
}

// TestRunRejectsNonUTF8Stdin requires the stdin prompt to be held to the
// same encoding rule as a task file: it is one of the three input
// mechanisms, so a bad byte on stdin is the same usage error.
func TestRunRejectsNonUTF8Stdin(t *testing.T) {
	installConfigPath(t, filepath.Join(t.TempDir(), "config.json"))
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1"}, "\xff\xfe bad")
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %q", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "not valid UTF-8") {
		t.Fatalf("stderr = %q, want the encoding rejection", stderr)
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
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("done"))
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "one", "--writes", "src/a", "--task", "two", "--writes", "src/b"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if strings.Contains(stderr, "share the writable current workspace") {
		t.Fatalf("stderr printed the shared-workspace warning: %q", stderr)
	}
	requireWritesTail(t, stdout, fakePiDoneLines(2))
}

func TestRunWritesKeepsSharedWorkspaceWarningWhenNoTaskDeclares(t *testing.T) {
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("done"))
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "one", "--task", "two"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	// No task declared, so the run is not contracted at all and the warning
	// must stay. A partial declaration can no longer reach this path: it is
	// a usage error before any worker starts.
	if count := strings.Count(stderr, "pi-worker: warning: 2 workers share the writable current workspace; tasks must use disjoint files"); count != 1 {
		t.Fatalf("warning count = %d, want 1: %q", count, stderr)
	}
	requireChangesTail(t, stdout, fakePiDoneLines(2))
}

// TestRunBackgroundWritesKeepsSharedWorkspaceWarningWhenNoTaskDeclares
// requires the background path to accept the same run the foreground
// accepts: two tasks with no --writes anywhere is legal, so the command
// reaches the start and prints the shared-workspace warning exactly once.
// The role program is a path that does not exist, so the start itself is
// refused before any supervisor exists — no real supervisor is created
// for this test — and the warning this test is about is printed before
// that refusal, exactly as it is before a successful start.
func TestRunBackgroundWritesKeepsSharedWorkspaceWarningWhenNoTaskDeclares(t *testing.T) {
	manager, err := background.NewManager(t.TempDir(), t.TempDir(), 2)
	if err != nil {
		t.Fatalf("background.NewManager: %v", err)
	}
	originalManager, originalRole := newBackgroundManager, backgroundRoleExecutable
	newBackgroundManager = func(string, int) (*background.Manager, error) { return manager, nil }
	backgroundRoleExecutable = filepath.Join(t.TempDir(), "no-such-supervisor")
	t.Cleanup(func() {
		newBackgroundManager, backgroundRoleExecutable = originalManager, originalRole
	})

	code, stdout, stderr := runCLI(t, []string{"run", "--background", "--model", "acme/m-1", "--task", "one", "--task", "two"}, "")
	if code == 0 {
		t.Fatalf("exit = 0 for a start whose supervisor could not be spawned; stdout = %q", stdout)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty for a start that was not accepted", stdout)
	}
	if count := strings.Count(stderr, "pi-worker: warning: 2 workers share the writable current workspace; tasks must use disjoint files"); count != 1 {
		t.Fatalf("warning count = %d, want 1: %q", count, stderr)
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
	args := []string{"--model", "acme/m-1", "--task-file", firstPath, "--writes", "internal/run,docs/a.md", "--task-file", secondPath, "--writes", "internal/cli"}
	_, tasks := mustResolveRun(t, args, "")
	if len(tasks) != 2 || tasks[0].Prompt != "first task" || tasks[1].Prompt != "second task" {
		t.Fatalf("tasks = %#v, want the two task files in order", tasks)
	}

	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("done"))
	code, stdout, stderr := runCLI(t, append([]string{"run"}, args...), "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if strings.Contains(stderr, "share the writable current workspace") {
		t.Fatalf("stderr printed the shared-workspace warning: %q", stderr)
	}
	requireWritesTail(t, stdout, fakePiDoneLines(2))
}

func TestRunWritesOverlapRejectedBeforeAnyWorkerStarts(t *testing.T) {
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "one", "--writes", "src/a", "--task", "two", "--writes", "src/a/b.go"}, "")
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %q", code, stderr)
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
	// "docs/a.md, src/x" must reach validation as the same paths as
	// "docs/a.md,src/x": the space after the comma is formatting, not part
	// of the path, so the overlap with task two's "src/x" is still rejected
	// before any worker starts, as a usage error exiting 2.
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "one", "--writes", "docs/a.md, src/x", "--task", "two", "--writes", "src/x"}, "")
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %q", code, stderr)
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
	useFakePi(t, backgroundHappyScript("done"))
	for _, args := range [][]string{
		{"run", "--model", "acme/m-1", "--writes", "file.txt", "--task", "go"},
		{"run", "--model", "acme/m-1", "--writes", "file.txt", "--task-file", taskFile},
	} {
		code, stdout, stderr := runCLI(t, args, "")
		if code != 0 {
			t.Fatalf("%v: exit = %d, want 0; stderr = %q", args, code, stderr)
		}
		if withoutRunLine(t, stderr) != "" {
			t.Fatalf("%v: stderr = %q", args, stderr)
		}
		want := fakePiDoneLines(1) +
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
	useFakePi(t, backgroundHappyScript("done"))
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--writes", "file.txt"}, "do it")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if withoutRunLine(t, stderr) != "" {
		t.Fatalf("stderr = %q", stderr)
	}
	want := fakePiDoneLines(1) +
		"changes: 0 files, +0/-0\n" +
		"writes: ok\n" +
		"outcome=completed\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	_, tasks := mustResolveRun(t, []string{"--model", "acme/m-1", "--writes", "file.txt"}, "do it")
	if len(tasks) != 1 || tasks[0].Prompt != "do it" {
		t.Fatalf("tasks = %#v, want the stdin prompt", tasks)
	}
	if !tasks[0].Writes.Declared || len(tasks[0].Writes.Paths) != 1 || tasks[0].Writes.Paths[0] != "file.txt" {
		t.Fatalf("writes = %#v, want file.txt bound to the stdin task", tasks[0].Writes)
	}
}

func TestRunWritesBeforeMultipleTasksRejectedWithRemedy(t *testing.T) {
	// A --writes that precedes more than one task has no knowable
	// target, so the run stays rejected — but the message must say what
	// to do, not only what is wrong: the declaration has to name its
	// task by following it.
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
}

func TestRunWritesTwiceForSingleTaskStillRejectedInAnyPosition(t *testing.T) {
	// The once-per-task limit is unchanged: a declaration that arrives
	// twice for the same task is rejected with the existing error,
	// whatever positions the two occurrences took — pending colliding
	// with positional, two pendings, and two pendings around a stdin
	// prompt.
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
}

func TestRunWritesNothingDeclarationAcceptedBeforeSingleTask(t *testing.T) {
	// --writes "" behaves identically wherever it appears: placed before
	// the single task it must still declare the writes-nothing set,
	// which the "writes: ok" verdict on a clean workspace proves.
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("done"))
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--writes", "", "--task", "go"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if withoutRunLine(t, stderr) != "" {
		t.Fatalf("stderr = %q", stderr)
	}
	want := fakePiDoneLines(1) +
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
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("done"))
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "one", "--writes", "src/a", "--task", "two", "--writes", ""}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if strings.Contains(stderr, "share the writable current workspace") {
		t.Fatalf("stderr printed the shared-workspace warning: %q", stderr)
	}
	requireWritesTail(t, stdout, fakePiDoneLines(2))
}

func TestRunSuccessHuman(t *testing.T) {
	opts, tasks := mustResolveRun(t, []string{"--model", "acme/m-1", "--task", "fix the bug"}, "")
	if len(tasks) != 1 || tasks[0].Model != "acme/m-1" || tasks[0].Prompt != "fix the bug" {
		t.Fatalf("tasks = %#v", tasks)
	}
	if opts.timeout != 30*time.Minute {
		t.Fatalf("timeout = %v, want the 30m default", opts.timeout)
	}

	workspace := newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("All done."))
	metaPath := filepath.Join(t.TempDir(), "meta.json")
	t.Setenv("FAKEPI_META", metaPath)
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "fix the bug"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	requireChangesTail(t, stdout, "worker 1 [model=acme/m-1 thinking=medium]: All done.\n")
	if withoutRunLine(t, stderr) != "" {
		t.Fatalf("stderr = %q", stderr)
	}
	if got, want := fakePiCwd(t, metaPath), resolvedDir(t, workspace); got != want {
		t.Fatalf("worker ran in %q, want the current directory %q", got, want)
	}

	// The default execution budget is the one the worker ran under.
	code, stdout, stderr = runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "fix the bug", "--json"}, "")
	if code != 0 {
		t.Fatalf("json exit = %d, want 0; stderr = %q", code, stderr)
	}
	if got := workerExecutionTimeout(t, stdout, 0); got != "30m0s" {
		t.Fatalf("worker executionTimeout = %q, want the 30m default", got)
	}
}

func TestRunThinkingPropagatesAndLabelsHumanOutput(t *testing.T) {
	_, tasks := mustResolveRun(t, []string{"--model", "acme/m-1", "--thinking=max", "--task", "fix the bug"}, "")
	if len(tasks) != 1 || tasks[0].ThinkingLevel != pi.ThinkingMax {
		t.Fatalf("tasks = %#v, want thinking max", tasks)
	}
	code, stdout, stderr := renderRun(t, finishedRun(contracts.RunCompleted, pi.WorkerResult{
		Model:                  "acme/m-1",
		RequestedThinkingLevel: pi.ThinkingMax,
		ThinkingLevel:          pi.ThinkingMax,
		Status:                 pi.StatusCompleted,
		Explanation:            "All done.",
	}), false)
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	requireChangesTail(t, stdout, "worker 1 [model=acme/m-1 thinking=max]: All done.\n")
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
			_, tasks := mustResolveRun(t, []string{"--model", "acme/m-1", "--thinking", string(level), "--task", "go"}, "")
			if got := tasks[0].ThinkingLevel; got != level {
				t.Fatalf("thinking = %q, want %q", got, level)
			}
		})
	}
}

func TestRunPerTaskModelsReachOwnWorkers(t *testing.T) {
	// Three tasks, three different models, one run: every worker
	// receives its own task's model, asserted per worker rather than in
	// aggregate.
	_, tasks := mustResolveRun(t, []string{"--task", "one", "--model", "acme/m-1", "--task", "two", "--model", "acme/m-2", "--task", "three", "--model", "acme/m-3"}, "")
	if len(tasks) != 3 {
		t.Fatalf("tasks = %#v, want three", tasks)
	}
	for i, want := range []string{"acme/m-1", "acme/m-2", "acme/m-3"} {
		if tasks[i].Model != want {
			t.Fatalf("worker %d model = %q, want %q", i+1, tasks[i].Model, want)
		}
	}
}

func TestRunTaskThinkingLevelsDoNotLeak(t *testing.T) {
	// Two tasks on the same model at different thinking levels: the
	// levels bind to their own tasks and do not leak into one another.
	_, tasks := mustResolveRun(t, []string{"--model", "acme/m-1", "--task", "one", "--thinking", "low", "--task", "two", "--thinking", "max"}, "")
	if got := tasks[0].ThinkingLevel; got != pi.ThinkingLow {
		t.Fatalf("worker 1 thinking = %q, want low", got)
	}
	if got := tasks[1].ThinkingLevel; got != pi.ThinkingMax {
		t.Fatalf("worker 2 thinking = %q, want max", got)
	}
}

func TestRunTaskWithoutModelFallsBackToRunLevelModel(t *testing.T) {
	// A task with no --model of its own falls back to the run-level
	// --model; a task with its own keeps it.
	_, tasks := mustResolveRun(t, []string{"--model", "acme/m-1", "--task", "one", "--task", "two", "--model", "acme/m-2"}, "")
	if got := tasks[0].Model; got != "acme/m-1" {
		t.Fatalf("worker 1 model = %q, want the run-level acme/m-1", got)
	}
	if got := tasks[1].Model; got != "acme/m-2" {
		t.Fatalf("worker 2 model = %q, want its own acme/m-2", got)
	}
}

func TestRunTaskThinkingOffStaysOff(t *testing.T) {
	// A task --thinking of "off" is an explicit level, not unset: it
	// must not fall back to the run-level thinking.
	_, tasks := mustResolveRun(t, []string{"--model", "acme/m-1", "--thinking", "max", "--task", "go", "--thinking", "off"}, "")
	if got := tasks[0].ThinkingLevel; got != pi.ThinkingOff {
		t.Fatalf("thinking = %q, want off", got)
	}
}

func TestRunModelBeforeTasksIsRunLevelAcrossTasks(t *testing.T) {
	// A --model that precedes every task keeps its run-level meaning on
	// a multi-task run: every worker runs with it.
	_, tasks := mustResolveRun(t, []string{"--model", "acme/m-1", "--task", "one", "--task", "two"}, "")
	if len(tasks) != 2 {
		t.Fatalf("tasks = %#v, want two", tasks)
	}
	for i, task := range tasks {
		if task.Model != "acme/m-1" {
			t.Fatalf("worker %d model = %q, want run-level acme/m-1", i+1, task.Model)
		}
	}
}

func TestRunSecondModelForSameTaskRejectedBeforeAnyWorkerStarts(t *testing.T) {
	// A second --model bound to the same task is rejected with the
	// task-naming error, before any worker starts.
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
		{name: "no slash", value: "acme"},
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
			runLevelCode, runLevelStdout, runLevelStderr := runCLI(t, []string{"run", "--model", test.value, "--task", "go"}, "")
			if runLevelCode != 2 || runLevelStdout != "" {
				t.Fatalf("invalid run-level model = (%d, %q, %q)", runLevelCode, runLevelStdout, runLevelStderr)
			}
			code, stdout, taskStderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--model", test.value}, "")
			if code != 2 || stdout != "" {
				t.Fatalf("invalid per-task model = (%d, %q, %q)", code, stdout, taskStderr)
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

func TestRunNameRuleAcceptsColonAndSpaceInId(t *testing.T) {
	// The name rule is structural only: it accepts a name whose id carries
	// a colon or a space, because those are id contents. Whether the name
	// is usable is the catalog's answer, which the worker gives at run
	// time — this test pins only that the shape rule no longer refuses the
	// name before any worker starts.
	for _, value := range []string{"acme/m-1:free", "acme/mo del"} {
		t.Run(value, func(t *testing.T) {
			_, tasks := mustResolveRun(t, []string{"--model", value, "--task", "go", "--json"}, "")
			if len(tasks) != 1 || tasks[0].Model != value {
				t.Fatalf("tasks = %#v, want model %q", tasks, value)
			}
			code, stdout, stderr := renderRun(t, finishedRun(contracts.RunCompleted, pi.WorkerResult{Model: value, Status: pi.StatusCompleted, Explanation: "done"}), true)
			if code != 0 {
				t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
			}
			if stderr != "" {
				t.Fatalf("stderr = %q, want empty", stderr)
			}
			if output := decodeRunOutput(t, stdout); len(output.Workers) != 1 || output.Workers[0].Model != value {
				t.Fatalf("output workers = %#v, want model %q", output.Workers, value)
			}
		})
	}
}

func TestRunNameRuleRejectsEmptyHalves(t *testing.T) {
	// A name with no slash, an empty provider, or an empty id is still
	// refused by the name rule before any worker starts.
	for _, test := range []struct {
		name  string
		value string
	}{
		{name: "no slash", value: "acme"},
		{name: "empty provider", value: "/m-1"},
		{name: "empty id", value: "acme/"},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, []string{"run", "--model", test.value, "--task", "go"}, "")
			if code != 2 {
				t.Fatalf("exit = %d, want 2; stderr = %q", code, stderr)
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, "invalid model") {
				t.Fatalf("stderr = %q, want the invalid-model error", stderr)
			}
		})
	}
}

func TestRunInventedColonNameRefusedByCatalogNotByFormat(t *testing.T) {
	// An invented name carrying a colon passes the name rule — a colon in
	// an id is not a format error — so `run` must refuse it with the
	// catalog-membership answer, not a name-format answer. The real worker
	// drives the whole path: the catalog offers only the plain entry, the
	// requested colon name is not in it, and the run exits 3 with "not in
	// the available catalog" on stderr.
	useFakePi(t, &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}},
		},
	}})

	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1:free", "--task", "go", "--json"}, "")
	if code != 3 {
		t.Fatalf("exit = %d, want 3; stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "not in the available catalog") {
		t.Fatalf("stderr = %q, want the catalog-membership answer", stderr)
	}
	if strings.Contains(stderr, "invalid model selector") || strings.Contains(stderr, "invalid model") {
		t.Fatalf("stderr = %q, want no name-format answer", stderr)
	}
	output := decodeRunOutput(t, stdout)
	if output.Outcome != contracts.OutcomeWorkersUnavailable || len(output.Workers) != 1 || output.Workers[0].Status != pi.StatusUnavailable {
		t.Fatalf("output = %#v, want one unavailable worker", output)
	}
	if !strings.Contains(output.Workers[0].Error, "not in the available catalog") || strings.Contains(output.Workers[0].Error, "invalid model selector") {
		t.Fatalf("worker error = %q, want the catalog-membership answer", output.Workers[0].Error)
	}
}

func TestRunThinkingFallbackWarnsAndKeepsSuccessfulExit(t *testing.T) {
	warning := "requested thinking=max unavailable; continuing with Pi default thinking=medium"
	result := finishedRun(contracts.RunCompleted, pi.WorkerResult{
		Model:                  "acme/m-1",
		RequestedThinkingLevel: pi.ThinkingMax,
		ThinkingLevel:          pi.ThinkingMedium,
		ThinkingFallback:       true,
		Warning:                warning,
		Status:                 pi.StatusCompleted,
		Explanation:            "Completed with default effort.",
	})

	code, stdout, stderr := renderRun(t, result, true)
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
	code, stdout, stderr := renderRun(t, finishedRun(contracts.RunCompleted, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "JSON answer"}), true)
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
}

func TestRunVerifiedPiVersionProbesOnceBeforeWorkers(t *testing.T) {
	logPath := installProcessVersionProbe(t, piversion.VerifiedVersion+"\n", "", 0)
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("ok"))
	// The version probe and every fake Pi append to the same log, so the
	// log's order is the order they ran in.
	t.Setenv("FAKEPI_LOG", logPath)

	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "one", "--task", "two", "--task", "three"}, "")
	if code != 0 || stdout == "" || strings.Contains(stderr, "Pi version") {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if got := versionProbeCount(t, logPath); got != 1 {
		t.Fatalf("version probe count = %d, want 1", got)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.HasPrefix(string(data), "version\n") {
		t.Fatalf("log = %q: a worker started before the version probe", data)
	}
	if got := strings.Count(string(data), `"type":"prompt"`); got != 3 {
		t.Fatalf("worker prompts = %d, want 3", got)
	}
}

func TestRunUnverifiedPiVersionWarnsOnceAndKeepsJSONClean(t *testing.T) {
	logPath := installProcessVersionProbe(t, "0.99.0\n", "child-secret-must-not-leak", 0)
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("JSON answer"))

	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--json"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if got := versionProbeCount(t, logPath); got != 1 {
		t.Fatalf("version probe count = %d, want 1", got)
	}
	const wantWarning = "pi-worker: warning: Pi version 0.99.0 is unverified; verified version is " + piversion.VerifiedVersion + "; continuing\n"
	if withoutRunLine(t, stderr) != wantWarning || strings.Contains(stderr, "child-secret-must-not-leak") {
		t.Fatalf("stderr = %q", stderr)
	}
	_ = decodeRunOutput(t, stdout)
	if strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
		t.Fatalf("stdout has multiple JSON documents: %q", stdout)
	}
}

func TestRunMalformedPiVersionWarnsAndKeepsJSONClean(t *testing.T) {
	installProcessVersionProbe(t, "pi 0.84.1\n", "", 0)
	newGitWorkspace(t)
	useFakePi(t, backgroundHappyScript("ok"))

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
	newGitWorkspace(t)
	// The fake Pi's catalog offers acme/m-1 only, so the worker for
	// acme/m-2 is unavailable: the run's own readiness exit.
	useFakePi(t, backgroundHappyScript("ok"))

	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-2", "--task", "go", "--json"}, "")
	if code != 3 {
		t.Fatalf("exit = %d, want existing readiness exit 3; stderr = %q", code, stderr)
	}
	_ = decodeRunOutput(t, stdout)
	if !strings.Contains(stderr, "pi-worker: warning: Pi version") || !strings.Contains(stderr, "not in the available catalog") {
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
	_, tasks := mustResolveRun(t, []string{"--model", "acme/m-1", "--task-file", path}, "")
	if len(tasks) != 1 || tasks[0].Prompt != "fix from file" {
		t.Fatalf("tasks = %#v, want the task file's text", tasks)
	}
}

func TestRunStdinTask(t *testing.T) {
	_, tasks := mustResolveRun(t, []string{"--model", "acme/m-1"}, "task from stdin")
	if len(tasks) != 1 || tasks[0].Prompt != "task from stdin" {
		t.Fatalf("tasks = %#v, want the stdin text", tasks)
	}
}

func TestRunThreeTasksHumanSuccessIsOrderedAndConcurrent(t *testing.T) {
	args := []string{"--model", "acme/m-1", "--task", "one", "--task", "two", "--task", "three"}
	_, tasks := mustResolveRun(t, args, "")
	for i, want := range []string{"one", "two", "three"} {
		if tasks[i].Prompt != want {
			t.Fatalf("worker %d prompt = %q, want %q", i+1, tasks[i].Prompt, want)
		}
	}

	// The summary lists the workers in input order, whatever order they
	// finished in.
	code, stdout, stderr := renderRun(t, finishedRun(contracts.RunCompleted,
		pi.WorkerResult{Status: pi.StatusCompleted, Model: "acme/m-1", Explanation: "first done"},
		pi.WorkerResult{Status: pi.StatusCompleted, Model: "acme/m-1", Explanation: "second done"},
		pi.WorkerResult{Status: pi.StatusCompleted, Model: "acme/m-1", Explanation: "third done"},
	), false)
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	requireChangesTail(t, stdout, "worker 1: first done\nworker 2: second done\nworker 3: third done\n")

	newGitWorkspace(t)
	useFakePi(t, heldHappyScript("done", 1500))
	code, stdout, stderr = runCLI(t, append([]string{"run"}, args...), "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if count := strings.Count(stderr, "pi-worker: warning:"); count != 1 {
		t.Fatalf("stderr warning count = %d, want 1", count)
	}
	requireChangesTail(t, stdout, fakePiDoneLines(3))
	// Each fake Pi holds its prompt: every prompt arriving before any
	// worker asked for its answer means all three were in flight at once.
	if got := fakePiRequestOrder(t, os.Getenv("FAKEPI_LOG")); got != "PPPAAA" {
		t.Fatalf("request order = %q, want all three prompts before any answer (PPPAAA)", got)
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

	_, tasks := mustResolveRun(t, []string{"--model", "acme/m-1", "--task-file", firstPath, "--task-file", secondPath}, "")
	if len(tasks) != 2 || tasks[0].Prompt != "first task" || tasks[1].Prompt != "second task" {
		t.Fatalf("tasks = %#v, want the two task files in order", tasks)
	}
	var warnings bytes.Buffer
	warnSharedWorkspace(tasks, &warnings)
	if count := strings.Count(warnings.String(), "pi-worker: warning:"); count != 1 {
		t.Fatalf("stderr warning count = %d, want 1", count)
	}
	code, stdout, stderr := renderRun(t, finishedRun(contracts.RunCompleted,
		pi.WorkerResult{Status: pi.StatusCompleted, Model: "acme/m-1", Explanation: "first file done"},
		pi.WorkerResult{Status: pi.StatusCompleted, Model: "acme/m-1", Explanation: "second file done"},
	), false)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	requireChangesTail(t, stdout, "worker 1: first file done\nworker 2: second file done\n")
}

func TestRunTwoTaskJSONResultOrder(t *testing.T) {
	_, tasks := mustResolveRun(t, []string{"--model", "acme/m-1", "--task", "one", "--task", "two", "--json"}, "")
	var warnings bytes.Buffer
	warnSharedWorkspace(tasks, &warnings)
	if strings.Count(warnings.String(), "pi-worker: warning:") != 1 {
		t.Fatalf("stderr = %q", warnings.String())
	}

	code, stdout, stderr := renderRun(t, finishedRun(contracts.RunCompleted,
		pi.WorkerResult{Status: pi.StatusCompleted, Model: "acme/m-1", Explanation: "json one"},
		pi.WorkerResult{Status: pi.StatusCompleted, Model: "acme/m-1", Explanation: "json two"},
	), true)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
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
			result := finishedRun(contracts.RunFailed, test.res)
			code, stdout, stderr := renderRun(t, result, false)
			if code != test.want {
				t.Fatalf("human exit = %d, want %d", code, test.want)
			}
			if !strings.Contains(stderr, test.res.Error) {
				t.Fatalf("human stderr = %q, want detail %q", stderr, test.res.Error)
			}
			lines := strings.Split(strings.TrimSpace(stdout), "\n")
			if len(lines) != 2 || !strings.HasPrefix(lines[0], "changes: ") || !strings.HasPrefix(lines[1], "outcome=") {
				t.Fatalf("human stdout = %q, want the changes: line and an outcome= line", stdout)
			}

			code, stdout, stderr = renderRun(t, result, true)
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

	// A failed run still carries its one change-manifest line and the
	// final outcome line: the manifest is measured on every terminal
	// status, and the outcome word names the exit code. The fake Pi's
	// catalog offers acme/m-1 only, so the worker for acme/m-2 is
	// unavailable.
	t.Run("failed run measures its changes", func(t *testing.T) {
		newGitWorkspace(t)
		useFakePi(t, backgroundHappyScript("ok"))
		code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-2", "--task", "x"}, "")
		if code != 3 {
			t.Fatalf("exit = %d, want 3; stderr = %q", code, stderr)
		}
		if stdout != "changes: 0 files, +0/-0\noutcome=workers-unavailable\n" {
			t.Fatalf("human stdout = %q, want the changes: line and the outcome= line", stdout)
		}
	})
}

func TestRunExitCodePartialAndLabeledErrors(t *testing.T) {
	result := finishedRun(contracts.RunPartial,
		pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusCompleted, Explanation: "primary done"},
		pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusFailed, Error: "agent failed"},
	)

	code, stdout, stderr := renderRun(t, result, false)
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

	code, stdout, stderr = renderRun(t, result, true)
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
	code, stdout, stderr := renderRun(t, finishedRun(contracts.RunFailed,
		pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusUnavailable, Error: "model unavailable"},
		pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusUnavailable, Error: "adapter unavailable"},
	), true)
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
	code, stdout, stderr := renderRun(t, finishedRun(contracts.RunFailed,
		pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusError, Error: "protocol error"},
		pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusUnavailable, Error: "model unavailable"},
	), true)
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
	opts, _ := mustResolveRun(t, []string{"--model", "acme/m-1", "--task", "x", "--timeout", "250ms"}, "")
	if opts.timeout != 250*time.Millisecond {
		t.Fatalf("timeout = %v, want 250ms", opts.timeout)
	}

	// The fake Pi holds its prompt far longer than the requested budget,
	// so the run ends on the budget, not on the worker's answer.
	newGitWorkspace(t)
	useFakePi(t, heldHappyScript("ok", 10000))
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "x", "--timeout", "250ms", "--json"}, "")
	if code != 7 {
		t.Fatalf("exit = %d, want 7; stderr = %q", code, stderr)
	}
	if got := workerExecutionTimeout(t, stdout, 0); got != "250ms" {
		t.Fatalf("worker executionTimeout = %q, want 250ms", got)
	}
	// The full 250ms execution budget starts when the admitted worker
	// starts, not when the CLI process begins.
	var document struct {
		Workers []struct {
			StartedAt  *time.Time `json:"startedAt"`
			FinishedAt *time.Time `json:"finishedAt"`
		} `json:"workers"`
	}
	if err := json.Unmarshal([]byte(stdout), &document); err != nil {
		t.Fatalf("decode json stdout: %v (%q)", err, stdout)
	}
	if len(document.Workers) != 1 || document.Workers[0].StartedAt == nil || document.Workers[0].FinishedAt == nil {
		t.Fatalf("workers = %q, want one worker with its start and finish", stdout)
	}
	if ran := document.Workers[0].FinishedAt.Sub(*document.Workers[0].StartedAt); ran < 225*time.Millisecond {
		t.Fatalf("worker ran %v, want the whole 250ms budget after it started", ran)
	}
}

func TestRunTimedOutHumanPrintsPartialTextOnStdout(t *testing.T) {
	// A timed-out worker that salvaged partial text: the result carries
	// the salvaged explanation, the error line still goes to stderr
	// exactly as before, and the salvaged text is printed on stdout in
	// full, marked (incomplete) so it cannot read as a finished answer.
	code, stdout, stderr := renderRun(t, finishedRun(contracts.RunTimedOut, pi.WorkerResult{
		Model:              "acme/m-1",
		Status:             pi.StatusTimedOut,
		Error:              "timed out",
		PartialExplanation: "partial text from the interrupted run",
	}), false)
	if code != 7 {
		t.Fatalf("exit = %d, want 7; stderr = %q", code, stderr)
	}
	requireChangesTail(t, stdout, "worker 1 (incomplete): partial text from the interrupted run\n")
	if !strings.Contains(stderr, "pi-worker: worker 1: timed out\n") {
		t.Fatalf("stderr = %q, want the timed-out error line", stderr)
	}
}

func TestRunTimedOutHumanPrintsNothingExtraWhenPartialTextEmpty(t *testing.T) {
	// A timed-out worker with no salvaged text prints nothing extra:
	// stdout stays exactly the change-manifest line followed by the
	// final outcome line, and the error line still names the timeout.
	code, stdout, stderr := renderRun(t, finishedRun(contracts.RunTimedOut, pi.WorkerResult{Model: "acme/m-1", Status: pi.StatusTimedOut, Error: "timed out"}), false)
	if code != 7 {
		t.Fatalf("exit = %d, want 7; stderr = %q", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "changes: ") || lines[1] != "outcome=timeout" {
		t.Fatalf("human stdout = %q, want one changes: line then exactly outcome=timeout", stdout)
	}
	if strings.Contains(stdout, "(incomplete)") {
		t.Fatalf("human stdout = %q, want no (incomplete) line", stdout)
	}
	if !strings.Contains(stderr, "pi-worker: worker 1: timed out\n") {
		t.Fatalf("stderr = %q, want the timed-out error line", stderr)
	}
}

func TestRunCompletedHumanOutputUnchanged(t *testing.T) {
	// A completed worker's output is byte-identical to before the
	// salvage renderer: only the plain explanation line, even when the
	// result also carries partial text — which the worker never emits
	// on completion, but the renderer must not depend on that guard.
	code, stdout, stderr := renderRun(t, finishedRun(contracts.RunCompleted, pi.WorkerResult{
		Model:              "acme/m-1",
		Status:             pi.StatusCompleted,
		Explanation:        "All done.",
		PartialExplanation: "must never print",
	}), false)
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	requireChangesTail(t, stdout, "worker 1: All done.\n")
	if strings.Contains(stdout, "(incomplete)") {
		t.Fatalf("stdout = %q, want no (incomplete) line for a completed worker", stdout)
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

// TestMainNamesUnknownTopLevelCommand pins that an unknown top-level
// command is named before the usage text on stderr, through both the
// public Main entry point and the private mainWithContext test seam. An
// empty argv is not an unknown command: it prints usage alone.
func TestMainNamesUnknownTopLevelCommand(t *testing.T) {
	tests := []struct {
		name string
		args []string
		// wantLine is the exact diagnostic line expected at the start of
		// stderr. Empty means the arguments are not an unknown command
		// and stderr must not carry the diagnostic at all.
		wantLine string
	}{
		{name: "unknown flag", args: []string{"--bogus"}, wantLine: "pi-worker: unknown command \"--bogus\"\n"},
		{name: "unknown word", args: []string{"bogus"}, wantLine: "pi-worker: unknown command \"bogus\"\n"},
		{name: "empty argv", args: []string{}},
	}
	helpers := []struct {
		name string
		run  func(*testing.T, []string) (int, string, string)
	}{
		{name: "Main", run: func(t *testing.T, args []string) (int, string, string) {
			return runCLI(t, args, "")
		}},
		{name: "mainWithContext", run: func(t *testing.T, args []string) (int, string, string) {
			return runCLIWithContext(t, context.Background(), args, "")
		}},
	}
	for _, helper := range helpers {
		for _, tc := range tests {
			t.Run(helper.name+"/"+tc.name, func(t *testing.T) {
				code, stdout, stderr := helper.run(t, tc.args)
				if code != 2 {
					t.Fatalf("code = %d, want 2; stderr = %q", code, stderr)
				}
				if stdout != "" {
					t.Fatalf("stdout = %q, want empty", stdout)
				}
				if !strings.Contains(stderr, "usage: pi-worker version [--json]") {
					t.Fatalf("stderr = %q, want the usage text", stderr)
				}
				if tc.wantLine == "" {
					if strings.Contains(stderr, "unknown command") {
						t.Fatalf("stderr = %q, want no unknown command diagnostic", stderr)
					}
					return
				}
				if !strings.HasPrefix(stderr, tc.wantLine) {
					t.Fatalf("stderr = %q, want it to start with %q", stderr, tc.wantLine)
				}
			})
		}
	}
}

func TestMainAcceptsConventionalVersionAndHelpFlags(t *testing.T) {
	helpers := []struct {
		name string
		run  func(*testing.T, []string) (int, string, string)
	}{
		{name: "Main", run: func(t *testing.T, args []string) (int, string, string) {
			return runCLI(t, args, "")
		}},
		{name: "mainWithContext", run: func(t *testing.T, args []string) (int, string, string) {
			return runCLIWithContext(t, context.Background(), args, "")
		}},
	}
	for _, helper := range helpers {
		t.Run(helper.name, func(t *testing.T) {
			t.Run("--version", func(t *testing.T) {
				code, stdout, stderr := helper.run(t, []string{"--version"})
				if code != 0 {
					t.Fatalf("code = %d, want 0; stderr = %q", code, stderr)
				}
				if stderr != "" {
					t.Fatalf("stderr = %q, want empty", stderr)
				}
				wantCode, wantStdout, wantStderr := helper.run(t, []string{"version"})
				if wantCode != 0 || wantStderr != "" {
					t.Fatalf("version baseline: code = %d, stderr = %q", wantCode, wantStderr)
				}
				if stdout != wantStdout {
					t.Fatalf("stdout = %q, want the version stdout %q", stdout, wantStdout)
				}
				if !strings.HasPrefix(stdout, "pi-worker ") {
					t.Fatalf("stdout = %q, want it to start with %q", stdout, "pi-worker ")
				}
			})
			t.Run("--version --json", func(t *testing.T) {
				code, stdout, stderr := helper.run(t, []string{"--version", "--json"})
				if code != 0 {
					t.Fatalf("code = %d, want 0; stderr = %q", code, stderr)
				}
				if stderr != "" {
					t.Fatalf("stderr = %q, want empty", stderr)
				}
				wantCode, wantStdout, wantStderr := helper.run(t, []string{"version", "--json"})
				if wantCode != 0 || wantStderr != "" {
					t.Fatalf("version --json baseline: code = %d, stderr = %q", wantCode, wantStderr)
				}
				if stdout != wantStdout {
					t.Fatalf("stdout = %q, want the version --json stdout %q", stdout, wantStdout)
				}
			})
			for _, arg := range []string{"-h", "--help"} {
				t.Run(arg, func(t *testing.T) {
					code, stdout, stderr := helper.run(t, []string{arg})
					if code != 0 {
						t.Fatalf("code = %d, want 0; stderr = %q", code, stderr)
					}
					if stderr != "" {
						t.Fatalf("stderr = %q, want empty", stderr)
					}
					if !strings.Contains(stdout, "usage: pi-worker version [--json]") {
						t.Fatalf("stdout = %q, want the usage text", stdout)
					}
					if strings.Contains(stdout, "unknown command") {
						t.Fatalf("stdout = %q, want no unknown command diagnostic", stdout)
					}
				})
			}
		})
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

func TestPrintRunLeftoversNamesEachProcess(t *testing.T) {
	var stderr bytes.Buffer
	printRunLeftovers([]run.LeftoverProcess{
		{PID: 88143, Name: "node"},
		{PID: 88144},
	}, &stderr)
	want := "pi-worker: warning: this run left processes running: node (pid 88143), pid 88144\n"
	if got := stderr.String(); got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}
}

func TestPrintRunLeftoversCapsAtTen(t *testing.T) {
	var stderr bytes.Buffer
	processes := make([]run.LeftoverProcess, 0, 12)
	for pid := 1; pid <= 12; pid++ {
		processes = append(processes, run.LeftoverProcess{PID: pid, Name: "p"})
	}
	printRunLeftovers(processes, &stderr)
	want := "pi-worker: warning: this run left processes running: p (pid 1), p (pid 2), p (pid 3), p (pid 4), p (pid 5), p (pid 6), p (pid 7), p (pid 8), p (pid 9), p (pid 10) and 2 more\n"
	if got := stderr.String(); got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}
}

func TestPrintRunLeftoversEmptyPrintsNothing(t *testing.T) {
	var stderr bytes.Buffer
	printRunLeftovers(nil, &stderr)
	if got := stderr.String(); got != "" {
		t.Fatalf("output = %q, want empty", got)
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

func TestRunConfiguredMaxModelWorkersSerializesForegroundTasks(t *testing.T) {
	// Save schema-2 config with MaxModelWorkers 1 and install it so the
	// admission gate derived beside the config file is test-scoped.
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.json")
	if err := config.Save(configPath, config.Config{SchemaVersion: 2, MaxModelWorkers: 1}); err != nil {
		t.Fatal(err)
	}
	installConfigPath(t, configPath)

	// Each fake Pi holds its prompt, so a second worker admitted while the
	// first is still in flight would reach its prompt before the first
	// asked for its answer.
	newGitWorkspace(t)
	useFakePi(t, heldHappyScript("done", 300))
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "first", "--task", "second"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	if got := fakePiRequestOrder(t, os.Getenv("FAKEPI_LOG")); got != "PAPA" {
		t.Fatalf("request order = %q, want one worker at a time (PAPA)", got)
	}
	// Both outputs must appear in input order.
	requireChangesTail(t, stdout, fakePiDoneLines(2))
	// The existing shared-workspace warning on stderr is allowed; reject
	// worker or internal error lines.
	for _, line := range strings.Split(withoutRunLine(t, stderr), "\n") {
		if line == "" {
			continue
		}
		if strings.Contains(line, "share the writable current workspace") {
			continue
		}
		t.Fatalf("unexpected stderr line: %q", line)
	}
}

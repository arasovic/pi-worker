//go:build darwin || linux

package background

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/arasovic/pi-worker/internal/testutil/fakepi/script"
	"github.com/shirou/gopsutil/v4/process"
)

// Worker-host child-mode environment variables. TestMain reads them to
// switch a spawned role-process test child out of its default behavior
// into one of the worker-host modes below. Unset or empty keeps the
// behavior every earlier role-process test relies on.

// workerHostExecuteEnv switches a roleWorkerHost child into the real
// production child-side worker-host handler (receiveWorkerHost).
const workerHostExecuteEnv = "PI_WORKER_BACKGROUND_TEST_WORKER_HOST_EXECUTE"

// workerHostStuckEnv makes a roleWorkerHost child ignore everything and
// live forever; workerHostStuckPIDFileEnv records its exact pid.
const workerHostStuckEnv = "PI_WORKER_BACKGROUND_TEST_WORKER_HOST_STUCK"

// workerHostStuckPIDFileEnv carries the pid-file path of the stuck mode.
const workerHostStuckPIDFileEnv = "PI_WORKER_BACKGROUND_TEST_WORKER_HOST_STUCK_PIDFILE"

// workerHostCrashEnv makes a roleWorkerHost child exit immediately with
// code 3 without reading anything.
const workerHostCrashEnv = "PI_WORKER_BACKGROUND_TEST_WORKER_HOST_CRASH"

// workerHostClosedResponseEnv makes a roleWorkerHost child close its
// response writer — so the parent reads EOF at once — and then ignore
// everything and live forever, exactly like the stuck mode. It reuses
// workerHostStuckPIDFileEnv to record its exact pid.
const workerHostClosedResponseEnv = "PI_WORKER_BACKGROUND_TEST_WORKER_HOST_CLOSED_RESPONSE"

// workerHostWriteGraceEnv bounds the child-side response write grace of
// the execute TestMain mode: the spawned child parses it in TestMain and
// sets its own workerHostWriteGrace, so real-subprocess tests never
// mutate the parent process's package global. Unset or empty keeps the
// production default.
const workerHostWriteGraceEnv = "PI_WORKER_BACKGROUND_TEST_WORKER_HOST_WRITE_GRACE"

// workerHostProtocolFailureEnv makes a roleWorkerHost child answer its
// one request with one malformed frame (an unknown kind) followed by one
// strictly valid completed terminal result frame, then exit 0 on its
// own. Adapter tests use it to prove a later terminal result never
// overrides an earlier protocol failure. Unset or empty keeps the
// ownership-EOF behavior every earlier role-process test relies on.
const workerHostProtocolFailureEnv = "PI_WORKER_BACKGROUND_TEST_WORKER_HOST_PROTOCOL_FAILURE"

// Supervisor-driver environment variables. TestMain reads them to switch
// a roleSupervisor child into the opt-in flow-test driver: after the real
// child-side supervisor start exchange it consumes its own prepared
// admission ticket and runs one real worker-host execution.
const (
	supervisorDriverOutcomeEnv = "PI_WORKER_BACKGROUND_TEST_SUPERVISOR_OUTCOME"
	supervisorDriverHoldEnv    = "PI_WORKER_BACKGROUND_TEST_SUPERVISOR_HOLD"
)

// fakePi build state: the fakepi helper binary is built once per test
// run, and TestMain removes its directory after the run.
var (
	fakePiBuildOnce sync.Once
	fakePiBinPath   string
	fakePiBinDir    string
	fakePiBuildErr  error
)

// buildFakePiOnce builds the fakepi helper binary once per test run.
func buildFakePiOnce() {
	fakePiBuildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "pi-worker-bg-fakepi-bin-*")
		if err != nil {
			fakePiBuildErr = fmt.Errorf("create fakepi build directory: %w", err)
			return
		}
		fakePiBinDir = dir
		fakePiBinPath = filepath.Join(dir, "fakepi")
		build := exec.Command("go", "build", "-o", fakePiBinPath, "github.com/arasovic/pi-worker/internal/testutil/fakepi")
		if out, err := build.CombinedOutput(); err != nil {
			fakePiBuildErr = fmt.Errorf("build fakepi: %v\n%s", err, out)
			_ = os.RemoveAll(dir)
			fakePiBinDir = ""
			return
		}
	})
}

// removeFakePiBuildDir removes the per-run fakepi build directory; it is
// called by TestMain after the test run.
func removeFakePiBuildDir() {
	if fakePiBinDir != "" {
		_ = os.RemoveAll(fakePiBinDir)
	}
}

// fakePiBin returns the built fakepi helper binary path, building it on
// first use.
func fakePiBin(t *testing.T) string {
	t.Helper()
	buildFakePiOnce()
	if fakePiBuildErr != nil {
		t.Fatalf("build fakepi: %v", fakePiBuildErr)
	}
	return fakePiBinPath
}

// setupFakePiEnv points FAKEPI_SCRIPT and FAKEPI_LOG at a fresh script
// and request log for one test and returns the log path, mirroring the
// internal/pi test helpers.
func setupFakePiEnv(t *testing.T, scriptConfig *script.Script) string {
	t.Helper()
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "script.json")
	if scriptConfig != nil {
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
		if err := os.WriteFile(scriptPath, data, 0o600); err != nil {
			t.Fatalf("write script: %v", err)
		}
	}
	logPath := filepath.Join(dir, "requests.log")
	sequenceStatePath := filepath.Join(dir, "sequence-state.json")
	t.Setenv("FAKEPI_SCRIPT", scriptPath)
	t.Setenv("FAKEPI_LOG", logPath)
	t.Setenv("FAKEPI_SEQUENCE_STATE", sequenceStatePath)
	return logPath
}

// happyPathScript drives fakepi through the full worker lifecycle,
// mirroring the internal/pi happy-path script.
func happyPathScript(finalText string) *script.Script {
	return &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}},
		},
		"set_model": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"provider":"acme","id":"m-1"}`)}},
		},
		"get_state": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"model":{"provider":"acme","id":"m-1"},"thinkingLevel":"medium","isStreaming":false}`)}},
		},
		"prompt": {
			{Response: &script.Response{Success: true}},
			{Event: json.RawMessage(`{"type":"agent_start"}`)},
			{Event: json.RawMessage(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"The answer is 42."}]}}`)},
			{Event: json.RawMessage(`{"type":"turn_end","message":{},"toolResults":[]}`)},
			{Event: json.RawMessage(`{"type":"agent_end","messages":[],"willRetry":false}`)},
			{Event: json.RawMessage(`{"type":"agent_settled"}`)},
		},
		"get_last_assistant_text": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"text":"` + finalText + `"}`)}},
		},
	}}
}

// stuckPathScript drives fakepi through the full startup handshake and
// the prompt, then never settles: the run stays in flight until it is
// cancelled or times out, while fakepi itself returns to reading stdin
// (so it exits promptly on stdin EOF during cleanup).
func stuckPathScript() *script.Script {
	return &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}},
		},
		"set_model": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"provider":"acme","id":"m-1"}`)}},
		},
		"get_state": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"model":{"provider":"acme","id":"m-1"},"thinkingLevel":"medium","isStreaming":false}`)}},
		},
		"prompt": {
			{Response: &script.Response{Success: true}},
			{Event: json.RawMessage(`{"type":"agent_start"}`)},
		},
	}}
}

// readRequestLog decodes the fakepi request log into request types in
// order; a missing log yields nil.
func readRequestLog(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var types []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var req struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		types = append(types, req.Type)
	}
	return types
}

// countStrings counts occurrences of want in values.
func countStrings(values []string, want string) int {
	n := 0
	for _, value := range values {
		if value == want {
			n++
		}
	}
	return n
}

// waitRequestLog polls the fakepi request log until at least min entries
// are recorded and returns their request types in order.
func waitRequestLog(t *testing.T, path string, min int) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		types := readRequestLog(path)
		if len(types) >= min {
			return types
		}
		if time.Now().After(deadline) {
			t.Fatalf("request log %s has %d entries, want %d: %v", path, len(types), min, types)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// readPIDFile polls path until it holds a positive pid and returns it.
func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid file %s never became readable: %v", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// processAlive reports whether a Unix process with the given pid exists.
func processAlive(pid int) bool {
	err := unix.Kill(pid, 0)
	return err == nil || err == unix.EPERM
}

// waitProcessGone polls until pid no longer exists. An orphaned process
// is reparented and reaped by init, so the poll tolerates a short zombie
// window.
func waitProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			// Diagnose a lingering descendant before cleanup kills it:
			// live vs zombie vs pid reuse, from /proc/<pid>/stat alone.
			diag := "stat unavailable"
			if runtime.GOOS == "linux" {
				raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
				if err != nil {
					diag = err.Error()
				} else if stat := strings.TrimSpace(string(raw)); stat != "" {
					diag = stat[:min(len(stat), 300)]
				}
			}
			t.Fatalf("process %d survived cleanup (/proc stat: %s)", pid, diag)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// workerHostFixture is one real-pipe worker-host exchange viewed from
// the parent side. pipes carries the child-side transport ends that
// receiveWorkerHost takes ownership of — the request reader on fd 3, the
// response writer on fd 4, and the ownership reader on fd 5. The other
// three ends are exactly the handles a parent adapter holds. childDone
// is set once a handler goroutine is started over the fixture, and the
// cleanup waits for it (bounded) before closing anything, so the close
// never races the handler's own pipes close on a failing-test path.
// Cleanup closes every end the handler has not already closed.
type workerHostFixture struct {
	pipes           *childRolePipes
	requestWriter   *os.File // parent -> host request pipe write end
	responseReader  *os.File // host -> parent response pipe read end
	ownershipWriter *os.File // parent -> host ownership pipe write end
	childFinished   <-chan struct{}
}

// newWorkerHostFixture creates one real os.Pipe per direction and wraps
// the child-side ends in the childRolePipes handed to the handler.
func newWorkerHostFixture(t *testing.T) *workerHostFixture {
	t.Helper()
	requestReader, requestWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("create request pipe: %v", err)
	}
	responseReader, responseWriter, err := os.Pipe()
	if err != nil {
		requestReader.Close()
		requestWriter.Close()
		t.Fatalf("create response pipe: %v", err)
	}
	ownershipReader, ownershipWriter, err := os.Pipe()
	if err != nil {
		requestReader.Close()
		requestWriter.Close()
		responseReader.Close()
		responseWriter.Close()
		t.Fatalf("create ownership pipe: %v", err)
	}
	fx := &workerHostFixture{
		pipes: &childRolePipes{
			requestReader:   requestReader,
			responseWriter:  responseWriter,
			ownershipReader: ownershipReader,
		},
		requestWriter:   requestWriter,
		responseReader:  responseReader,
		ownershipWriter: ownershipWriter,
	}
	t.Cleanup(fx.close)
	return fx
}

// close releases every fixture end still open, ignoring errors: the
// child ends are normally already closed by the handler. When a handler
// goroutine was started over the fixture, the close first waits for it
// (bounded) so the handler's own pipes close finishes before this close
// reads the pipes struct; a handler stuck beyond the bound leaves the
// child ends it still owns untouched and only the parent ends are
// closed. Tests that assert no worker-host leak call close explicitly
// once the handler finished — before requireNoWorkerHostLeak, so the
// descriptor check never sees the fixture's own parent ends — and the
// t.Cleanup registration below then only backstops failing tests.
func (fx *workerHostFixture) close() {
	if fx.childFinished != nil {
		select {
		case <-fx.childFinished:
		case <-time.After(10 * time.Second):
			// The handler is stuck; the test is already failing. Never
			// touch the pipes the handler still owns.
			fx.pipes = nil
		}
	}
	if fx.pipes != nil {
		if fx.pipes.requestReader != nil {
			fx.pipes.requestReader.Close()
		}
		if fx.pipes.responseWriter != nil {
			fx.pipes.responseWriter.Close()
		}
		if fx.pipes.ownershipReader != nil {
			fx.pipes.ownershipReader.Close()
		}
	}
	if fx.requestWriter != nil {
		fx.requestWriter.Close()
	}
	if fx.responseReader != nil {
		fx.responseReader.Close()
	}
	if fx.ownershipWriter != nil {
		fx.ownershipWriter.Close()
	}
}

// workerHostChildOutcome carries one return of the child-side handler
// across a goroutine boundary.
type workerHostChildOutcome struct {
	exchange workerHostExchange
	err      error
}

// startWorkerHostChild runs receiveWorkerHost over the fixture's child
// ends on a fresh goroutine and returns the channel that receives the
// outcome. The fixture records a finished signal so its cleanup waits
// for the handler to return before closing anything.
func startWorkerHostChild(fx *workerHostFixture) <-chan workerHostChildOutcome {
	done := make(chan workerHostChildOutcome, 1)
	finished := make(chan struct{})
	fx.childFinished = finished
	go func() {
		defer close(finished)
		exchange, err := receiveWorkerHost(fx.pipes)
		done <- workerHostChildOutcome{exchange: exchange, err: err}
	}()
	return done
}

// waitWorkerHostChild blocks on done no longer than the bound and fails
// the test when the handler does not finish in time.
func waitWorkerHostChild(t *testing.T, done <-chan workerHostChildOutcome, bound time.Duration) workerHostChildOutcome {
	t.Helper()
	select {
	case out := <-done:
		return out
	case <-time.After(bound):
		t.Fatal("worker host child did not finish within the bounded wait")
		return workerHostChildOutcome{} // unreachable
	}
}

// sendWorkerHostRequest encodes req and writes exactly one request frame
// to the fixture request writer, which stays open.
func sendWorkerHostRequest(t *testing.T, fx *workerHostFixture, req workerHostRequest) {
	t.Helper()
	payload, err := encodeWorkerHostRequest(req)
	if err != nil {
		t.Fatalf("encode worker host request: %v", err)
	}
	if err := writeFrame(fx.requestWriter, payload, privateFrameLimit); err != nil {
		t.Fatalf("write worker host request frame: %v", err)
	}
}

// readWorkerHostFrame reads exactly one response frame from the fixture
// response reader under a bounded read deadline and decodes it strictly.
func readWorkerHostFrame(t *testing.T, fx *workerHostFixture) workerHostResponse {
	t.Helper()
	if err := fx.responseReader.SetReadDeadline(time.Now().Add(workerHostFixtureReadBound)); err != nil {
		t.Fatalf("set response read deadline: %v", err)
	}
	payload, err := readFrame(fx.responseReader, privateFrameLimit)
	if err != nil {
		t.Fatalf("read worker host response frame: %v", err)
	}
	frame, err := decodeWorkerHostResponse(payload)
	if err != nil {
		t.Fatalf("decode worker host response frame: %v", err)
	}
	return frame
}

// workerHostFixtureReadBound bounds one frame read in worker-host tests.
const workerHostFixtureReadBound = 15 * time.Second

// warmUpGopsutilSelfProbe resolves this process's gopsutil identity and
// boot time before a fixture test measures its leak baseline: gopsutil's
// one-time /proc/cpuinfo read must happen here, outside the measured
// exchange, so the descriptor some Linux runtimes keep on that file is
// already counted in the baseline.
func warmUpGopsutilSelfProbe(t *testing.T) {
	t.Helper()
	self, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		t.Fatalf("gopsutil self process: %v", err)
	}
	if _, err := self.CreateTime(); err != nil {
		t.Fatalf("gopsutil self boot time: %v", err)
	}
}

// requireNoWorkerHostLeak fails when goroutines or file descriptors grew
// across one worker-host operation: no goroutine or pipe end of a
// finished exchange may survive.
func requireNoWorkerHostLeak(t *testing.T, fdsBefore, gosBefore int) {
	t.Helper()
	deadline := time.Now().Add(1 * time.Second)
	for runtime.NumGoroutine() > gosBefore && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > gosBefore {
		t.Fatalf("goroutine growth across worker host exchange: before %d, after %d", gosBefore, got)
	}
	if fdsBefore > 0 {
		if after := countFDs(t); after > fdsBefore {
			t.Fatalf("fd growth across worker host exchange: before %d, after %d", fdsBefore, after)
		}
	}
}

// countWorkerHostStart wraps startRoleProcess so tests observe the exact
// spawned worker-host child and PID, mirroring captureHandoffStart.
func countWorkerHostStart(t *testing.T, proc **roleProcess, pid *int) {
	t.Helper()
	*proc = nil
	*pid = 0
	t.Cleanup(func() {
		p := *proc
		if p == nil || p.cmd.ProcessState != nil {
			return
		}
		if err := unix.Kill(*pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
			t.Errorf("cleanup kill worker host %d: %v", *pid, err)
		}
		if _, err := unix.Wait4(*pid, nil, 0, nil); err != nil && !errors.Is(err, unix.ECHILD) {
			t.Errorf("cleanup reap worker host %d: %v", *pid, err)
		}
	})
}

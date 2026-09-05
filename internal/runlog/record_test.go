package runlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/run"
)

// readRecord returns the bytes of the single record file in dir,
// failing the test on any deviation.
func readRecord(t *testing.T, dir string) []byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read record dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("record files = %d, want exactly 1", len(entries))
	}
	content, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	return content
}

// recordLines splits record bytes into lines, requiring the trailing
// newline every line must carry.
func recordLines(t *testing.T, content []byte) []string {
	t.Helper()
	if !bytes.HasSuffix(content, []byte("\n")) {
		t.Fatalf("record does not end in a newline: %q", content)
	}
	return strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
}

// decodeLine parses one record line into a JSON object.
func decodeLine(t *testing.T, line string) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal([]byte(line), &object); err != nil {
		t.Fatalf("line is not one JSON object: %v: %q", err, line)
	}
	return object
}

// TestStartDoesNotCreateRecordFileBeforeCreateTimeLookup pins the
// ordering inside Start that narrows the empty-file window: the
// record file must not exist while the creation-time lookup runs.
// The window between "the file exists" and "the start line is in
// it" is nothing but the single write that follows the open — a
// file that exists and is empty reads to the interrupted-run scan as
// a record not yet written, never as a corrupt one, and the
// watermark does not pass it. The seam checks the exact record path
// from inside the lookup and records what it saw.
func TestStartDoesNotCreateRecordFileBeforeCreateTimeLookup(t *testing.T) {
	dir := t.TempDir()
	startedAt := time.Date(2026, 8, 30, 4, 15, 30, 0, time.UTC)
	recordPath := filepath.Join(dir, RunID(startedAt)+".jsonl")
	var sawFileDuringLookup bool
	oldPidCreateTime := pidCreateTime
	pidCreateTime = func(pid int) (int64, error) {
		_, err := os.Stat(recordPath)
		switch {
		case err == nil:
			sawFileDuringLookup = true
		case !os.IsNotExist(err):
			t.Fatalf("stat record path from inside the lookup: %v", err)
		}
		return 1724998530123, nil
	}
	t.Cleanup(func() { pidCreateTime = oldPidCreateTime })

	recorder, err := Start(dir, startedAt, "/workspace", []run.Task{{Prompt: "p", Model: "acme/m-1"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer recorder.file.Close()
	if sawFileDuringLookup {
		t.Fatalf("record file already existed while the creation-time lookup ran")
	}
	// The other half of the pin: once the lookup has returned, the
	// record file exists and carries the start line.
	recordLines(t, readRecord(t, dir))
}

// TestStartWritesOneLine asserts Start writes exactly one line, parsing
// as JSON carrying the workspace, model, thinking level and prompt the
// test passed in. The recorder is deliberately never finished: the
// one-line state is the state of a run still in flight.
func TestStartWritesOneLine(t *testing.T) {
	dir := t.TempDir()
	recorder, err := Start(dir, time.Date(2026, 8, 30, 4, 15, 30, 0, time.UTC), "/tmp/workspace", []run.Task{{
		Prompt:        "build the thing",
		Model:         "acme/m-1",
		ThinkingLevel: pi.ThinkingMedium,
	}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer recorder.file.Close()

	lines := recordLines(t, readRecord(t, dir))
	if len(lines) != 1 {
		t.Fatalf("record lines = %d, want 1", len(lines))
	}
	line := decodeLine(t, lines[0])
	if line["event"] != "start" {
		t.Fatalf("event = %v, want start", line["event"])
	}
	if line["workspace"] != "/tmp/workspace" {
		t.Fatalf("workspace = %v, want /tmp/workspace", line["workspace"])
	}
	tasks, ok := line["tasks"].([]any)
	if !ok || len(tasks) != 1 {
		t.Fatalf("tasks = %#v, want one task", line["tasks"])
	}
	task := tasks[0].(map[string]any)
	if task["model"] != "acme/m-1" {
		t.Fatalf("model = %v, want acme/m-1", task["model"])
	}
	if task["thinkingLevel"] != "medium" {
		t.Fatalf("thinkingLevel = %v, want medium", task["thinkingLevel"])
	}
	if task["prompt"] != "build the thing" {
		t.Fatalf("prompt = %v, want build the thing", task["prompt"])
	}
	if task["promptTruncated"] != false {
		t.Fatalf("promptTruncated = %v, want false", task["promptTruncated"])
	}
	if task["writesDeclared"] != false {
		t.Fatalf("writesDeclared = %v, want false", task["writesDeclared"])
	}
}

// TestUnfinishedRecordHasOneLine asserts a record whose Finish was
// never called stays exactly one line: the missing finish line is how
// a later reader learns the run was interrupted.
func TestUnfinishedRecordHasOneLine(t *testing.T) {
	dir := t.TempDir()
	if _, err := Start(dir, time.Now(), "/workspace", []run.Task{{Prompt: "p", Model: "acme/m-1"}}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if lines := recordLines(t, readRecord(t, dir)); len(lines) != 1 {
		t.Fatalf("record lines = %d, want 1", len(lines))
	}
}

// TestFinishAppendsSecondLine asserts Finish appends exactly one more
// line and the file then has two, the second carrying the finish event
// and the result the test passed in, with no error field.
func TestFinishAppendsSecondLine(t *testing.T) {
	dir := t.TempDir()
	recorder, err := Start(dir, time.Now(), "/workspace", []run.Task{{Prompt: "p", Model: "acme/m-1"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	result := run.Result{SchemaVersion: 1, Status: "completed", Outcome: "completed"}
	if err := recorder.Finish(time.Date(2026, 8, 30, 4, 15, 31, 0, time.UTC), &result, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	lines := recordLines(t, readRecord(t, dir))
	if len(lines) != 2 {
		t.Fatalf("record lines = %d, want 2", len(lines))
	}
	finish := decodeLine(t, lines[1])
	if finish["event"] != "finish" {
		t.Fatalf("second line event = %v, want finish", finish["event"])
	}
	resultField, ok := finish["result"].(map[string]any)
	if !ok {
		t.Fatalf("result = %#v, want object", finish["result"])
	}
	if resultField["outcome"] != "completed" {
		t.Fatalf("result outcome = %v, want completed", resultField["outcome"])
	}
	if _, present := finish["error"]; present {
		t.Fatalf("error field present with a result: %#v", finish)
	}
}

// TestFinishCarriesRunError asserts the finish line carries the
// run-level error text when the run returned an error instead of a
// result, and no result field.
func TestFinishCarriesRunError(t *testing.T) {
	dir := t.TempDir()
	recorder, err := Start(dir, time.Now(), "/workspace", []run.Task{{Prompt: "p", Model: "acme/m-1"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := recorder.Finish(time.Now(), nil, errors.New("worker exploded")); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	lines := recordLines(t, readRecord(t, dir))
	if len(lines) != 2 {
		t.Fatalf("record lines = %d, want 2", len(lines))
	}
	finish := decodeLine(t, lines[1])
	if finish["error"] != "worker exploded" {
		t.Fatalf("error = %v, want worker exploded", finish["error"])
	}
	if _, present := finish["result"]; present {
		t.Fatalf("result field present with an error: %#v", finish)
	}
}

// TestRecordNeverCarriesMaterialContent asserts the content of a data
// file never enters the record: only its path, byte count and SHA-256
// do. The canary is exactly 20 bytes and its SHA-256 is written here as
// a literal so the expected hash is never computed by the same hash the
// production path runs.
func TestRecordNeverCarriesMaterialContent(t *testing.T) {
	const canary = "material-canary-9f3a"
	const wantHash = "de559d151efc76bb53731bfd773ad94f024a258a71f799ad3a5c9920159f1705"
	if len(canary) != 20 {
		t.Fatalf("canary length = %d, want 20", len(canary))
	}
	dir := t.TempDir()
	if _, err := Start(dir, time.Now(), "/workspace", []run.Task{{
		Prompt: "p",
		Model:  "acme/m-1",
		Data:   []run.DataFile{{Path: "x.md", Content: []byte(canary)}},
	}}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	content := readRecord(t, dir)
	if bytes.Contains(content, []byte(canary)) {
		t.Fatalf("record carries material content: %s", content)
	}
	if !bytes.Contains(content, []byte(`"byteCount":20`)) {
		t.Fatalf("record lacks the material's byte count: %s", content)
	}
	if !bytes.Contains(content, []byte(wantHash)) {
		t.Fatalf("record lacks the material's sha256: %s", content)
	}
}

// TestPromptOverCapIsTruncated asserts a 5000-byte prompt built from
// multi-byte characters is cut to at most 4096 bytes — without splitting
// a character — and marked truncated. The rune used is four bytes in
// UTF-8, so 1250 repetitions are exactly 5000 bytes and a naive byte
// cut would land mid-character.
func TestPromptOverCapIsTruncated(t *testing.T) {
	prompt := strings.Repeat("\U0001D11E", 1250)
	if len(prompt) != 5000 {
		t.Fatalf("prompt length = %d, want 5000", len(prompt))
	}
	dir := t.TempDir()
	if _, err := Start(dir, time.Now(), "/workspace", []run.Task{{Prompt: prompt, Model: "acme/m-1"}}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	lines := recordLines(t, readRecord(t, dir))
	tasks := decodeLine(t, lines[0])["tasks"].([]any)
	task := tasks[0].(map[string]any)
	recorded, ok := task["prompt"].(string)
	if !ok {
		t.Fatalf("prompt = %#v, want string", task["prompt"])
	}
	if len(recorded) > 4096 {
		t.Fatalf("recorded prompt = %d bytes, want at most 4096", len(recorded))
	}
	if !utf8.ValidString(recorded) {
		t.Fatalf("recorded prompt is not valid UTF-8: %q", recorded)
	}
	if task["promptTruncated"] != true {
		t.Fatalf("promptTruncated = %v, want true", task["promptTruncated"])
	}
}

// TestWorkerProcessAppendsThirdLine asserts WorkerProcess appends one
// line carrying the worker event, the worker id and the pid the test
// passed in, with the start and finish lines unchanged around it.
func TestWorkerProcessAppendsThirdLine(t *testing.T) {
	dir := t.TempDir()
	recorder, err := Start(dir, time.Date(2026, 8, 30, 4, 15, 30, 0, time.UTC), "/workspace", []run.Task{{Prompt: "p", Model: "acme/m-1"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	recorder.WorkerProcess(time.Date(2026, 8, 30, 4, 15, 35, 0, time.UTC), 2, 4832)
	result := run.Result{SchemaVersion: 1, Status: "completed", Outcome: "completed"}
	if err := recorder.Finish(time.Date(2026, 8, 30, 4, 15, 40, 0, time.UTC), &result, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	lines := recordLines(t, readRecord(t, dir))
	if len(lines) != 3 {
		t.Fatalf("record lines = %d, want 3", len(lines))
	}
	start := decodeLine(t, lines[0])
	if start["event"] != "start" {
		t.Fatalf("start line event = %v, want start", start["event"])
	}
	worker := decodeLine(t, lines[1])
	if worker["event"] != "worker" {
		t.Fatalf("second line event = %v, want worker", worker["event"])
	}
	if worker["runId"] != start["runId"] {
		t.Fatalf("worker runId = %v, start runId = %v", worker["runId"], start["runId"])
	}
	if worker["at"] != "2026-08-30T04:15:35Z" {
		t.Fatalf("at = %v, want 2026-08-30T04:15:35Z", worker["at"])
	}
	if worker["workerId"] != float64(2) {
		t.Fatalf("workerId = %v, want 2", worker["workerId"])
	}
	if worker["pid"] != float64(4832) {
		t.Fatalf("pid = %v, want 4832", worker["pid"])
	}
	finish := decodeLine(t, lines[2])
	if finish["event"] != "finish" {
		t.Fatalf("finish line event = %v, want finish", finish["event"])
	}
}

// TestStartLineOmitsNonZeroCreateTimeOnLookupError asserts a
// pidCreateTime call that returns a non-zero value together with an
// error still leaves the createTime key off the start line: the value
// is used only when the lookup succeeded. A failing call that also
// returns a value must not write that value as the writer's identity —
// the field's comment promises absence on a failed lookup, and the
// promise holds whatever the seam returns alongside the error.
func TestStartLineOmitsNonZeroCreateTimeOnLookupError(t *testing.T) {
	oldPidCreateTime := pidCreateTime
	pidCreateTime = func(pid int) (int64, error) { return 1724998530123, errors.New("no process table entry") }
	t.Cleanup(func() { pidCreateTime = oldPidCreateTime })

	dir := t.TempDir()
	if _, err := Start(dir, time.Now(), "/workspace", []run.Task{{Prompt: "p", Model: "acme/m-1"}}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	lines := recordLines(t, readRecord(t, dir))
	if bytes.Contains([]byte(lines[0]), []byte("createTime")) {
		t.Fatalf("start line carries createTime after a failed lookup: %s", lines[0])
	}
}

// TestWorkerLineOmitsNonZeroCreateTimeOnLookupError asserts a
// pidCreateTime call that returns a non-zero value together with an
// error still leaves the createTime key off the worker line: the value
// is used only when the lookup succeeded. The existing
// TestWorkerLineOmitsCreateTimeOnLookupError scripts the failure as a
// zero value with an error, which a sloppy implementation could pass
// by accident; this one scripts the value and the error together, the
// exact shape Finding B describes, so the absent key is forced by the
// error check and not by the zero.
func TestWorkerLineOmitsNonZeroCreateTimeOnLookupError(t *testing.T) {
	oldPidCreateTime := pidCreateTime
	pidCreateTime = func(pid int) (int64, error) { return 1724998530123, errors.New("no process table entry") }
	t.Cleanup(func() { pidCreateTime = oldPidCreateTime })

	dir := t.TempDir()
	recorder, err := Start(dir, time.Now(), "/workspace", []run.Task{{Prompt: "p", Model: "acme/m-1"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	recorder.WorkerProcess(time.Date(2026, 8, 30, 4, 15, 35, 0, time.UTC), 2, 4832)

	lines := recordLines(t, readRecord(t, dir))
	if bytes.Contains([]byte(lines[1]), []byte("createTime")) {
		t.Fatalf("worker line carries createTime after a failed lookup: %s", lines[1])
	}
}

// TestWorkerLineCarriesCreateTime asserts the worker line carries the
// process creation time the pidCreateTime seam returned, as the raw
// millisecond number — the exact-equality identity, never a formatted
// time. The expected value is a literal written here, never read back
// from the code under test, and its sub-second digits prove no
// precision was lost on the way to the line.
func TestWorkerLineCarriesCreateTime(t *testing.T) {
	const wantCreateTime = int64(1724998530123)
	oldPidCreateTime := pidCreateTime
	var sawPID int
	pidCreateTime = func(pid int) (int64, error) {
		sawPID = pid
		return wantCreateTime, nil
	}
	t.Cleanup(func() { pidCreateTime = oldPidCreateTime })

	dir := t.TempDir()
	recorder, err := Start(dir, time.Now(), "/workspace", []run.Task{{Prompt: "p", Model: "acme/m-1"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	recorder.WorkerProcess(time.Date(2026, 8, 30, 4, 15, 35, 0, time.UTC), 2, 4832)

	lines := recordLines(t, readRecord(t, dir))
	worker := decodeLine(t, lines[1])
	if worker["event"] != "worker" {
		t.Fatalf("line event = %v, want worker", worker["event"])
	}
	if worker["createTime"] != float64(wantCreateTime) {
		t.Fatalf("createTime = %v, want %d", worker["createTime"], wantCreateTime)
	}
	if sawPID != 4832 {
		t.Fatalf("pidCreateTime called with %d, want 4832", sawPID)
	}
}

// TestWorkerLineOmitsCreateTimeOnLookupError asserts a failed
// pidCreateTime lookup leaves the createTime key off the worker line
// entirely, never as a zero: the raw bytes of the line do not contain
// it, Finish reports no error about it, and the rest of the line is
// written unchanged. The absent field is the real, permanent shape of
// every record written before this field existed, so a later reader
// must be able to see "no field".
func TestWorkerLineOmitsCreateTimeOnLookupError(t *testing.T) {
	oldPidCreateTime := pidCreateTime
	pidCreateTime = func(pid int) (int64, error) { return 0, errors.New("no process table entry") }
	t.Cleanup(func() { pidCreateTime = oldPidCreateTime })

	dir := t.TempDir()
	recorder, err := Start(dir, time.Date(2026, 8, 30, 4, 15, 30, 0, time.UTC), "/workspace", []run.Task{{Prompt: "p", Model: "acme/m-1"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	recorder.WorkerProcess(time.Date(2026, 8, 30, 4, 15, 35, 0, time.UTC), 2, 4832)
	result := run.Result{SchemaVersion: 1, Status: "completed", Outcome: "completed"}
	if err := recorder.Finish(time.Date(2026, 8, 30, 4, 15, 40, 0, time.UTC), &result, nil); err != nil {
		t.Fatalf("Finish surfaced the failed lookup as an error: %v", err)
	}

	lines := recordLines(t, readRecord(t, dir))
	if len(lines) != 3 {
		t.Fatalf("record lines = %d, want 3", len(lines))
	}
	if bytes.Contains([]byte(lines[1]), []byte("createTime")) {
		t.Fatalf("worker line carries createTime after a failed lookup: %s", lines[1])
	}
	worker := decodeLine(t, lines[1])
	if worker["event"] != "worker" {
		t.Fatalf("line event = %v, want worker", worker["event"])
	}
	if worker["runId"] != decodeLine(t, lines[0])["runId"] {
		t.Fatalf("worker runId = %v, want the start line's %v", worker["runId"], decodeLine(t, lines[0])["runId"])
	}
	if worker["at"] != "2026-08-30T04:15:35Z" {
		t.Fatalf("at = %v, want 2026-08-30T04:15:35Z", worker["at"])
	}
	if worker["workerId"] != float64(2) {
		t.Fatalf("workerId = %v, want 2", worker["workerId"])
	}
	if worker["pid"] != float64(4832) {
		t.Fatalf("pid = %v, want 4832", worker["pid"])
	}
}

// TestWorkerLineSchemaVersionIsOne asserts the worker line still
// carries schemaVersion 1 after the createTime addition. Adding a
// field is additive — an old reader ignores the new key, a new reader
// tolerates its absence — so the record's own document version does
// not change.
func TestWorkerLineSchemaVersionIsOne(t *testing.T) {
	oldPidCreateTime := pidCreateTime
	pidCreateTime = func(pid int) (int64, error) { return 1724998530123, nil }
	t.Cleanup(func() { pidCreateTime = oldPidCreateTime })

	dir := t.TempDir()
	recorder, err := Start(dir, time.Now(), "/workspace", []run.Task{{Prompt: "p", Model: "acme/m-1"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	recorder.WorkerProcess(time.Now(), 1, 4832)

	lines := recordLines(t, readRecord(t, dir))
	worker := decodeLine(t, lines[1])
	if worker["event"] != "worker" {
		t.Fatalf("line event = %v, want worker", worker["event"])
	}
	if worker["schemaVersion"] != float64(1) {
		t.Fatalf("schemaVersion = %v, want 1", worker["schemaVersion"])
	}
}

// TestWorkerProcessNilRecorderIsNoOp asserts WorkerProcess on a nil
// Recorder is a no-op and panics on nothing.
func TestWorkerProcessNilRecorderIsNoOp(t *testing.T) {
	var recorder *Recorder
	recorder.WorkerProcess(time.Now(), 1, 4832)
}

// TestConcurrentWorkerProcessLinesRemainOneJSONObjectEach asserts many
// goroutines calling WorkerProcess at once leave a file whose every
// line still parses as one JSON object, with as many worker lines as
// there were calls. The mutex on Recorder is what makes this test pass
// under -race; without it the concurrent write-error slot access is a
// detected race and interleaved writes would corrupt lines.
func TestConcurrentWorkerProcessLinesRemainOneJSONObjectEach(t *testing.T) {
	dir := t.TempDir()
	recorder, err := Start(dir, time.Now(), "/workspace", []run.Task{{Prompt: "p", Model: "acme/m-1"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	at := time.Date(2026, 8, 30, 4, 15, 35, 0, time.UTC)
	const calls = 50
	var wg sync.WaitGroup
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			recorder.WorkerProcess(at, workerID, 1000+workerID)
		}(i)
	}
	wg.Wait()
	result := run.Result{SchemaVersion: 1, Status: "completed", Outcome: "completed"}
	if err := recorder.Finish(time.Now(), &result, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	lines := recordLines(t, readRecord(t, dir))
	if len(lines) != calls+2 {
		t.Fatalf("record lines = %d, want %d", len(lines), calls+2)
	}
	// decodeLine runs on every line and fails the test if any line is
	// not exactly one JSON object.
	if decodeLine(t, lines[0])["event"] != "start" {
		t.Fatalf("first line event = %v, want start", decodeLine(t, lines[0])["event"])
	}
	if decodeLine(t, lines[len(lines)-1])["event"] != "finish" {
		t.Fatalf("last line event = %v, want finish", decodeLine(t, lines[len(lines)-1])["event"])
	}
	workerLines := 0
	for _, line := range lines[1 : len(lines)-1] {
		object := decodeLine(t, line)
		if object["event"] != "worker" {
			t.Fatalf("middle line event = %v, want worker", object["event"])
		}
		workerLines++
	}
	if workerLines != calls {
		t.Fatalf("worker lines = %d, want %d", workerLines, calls)
	}
}

// TestDeclaredEmptyWritesStayDistinctFromNoDeclaration asserts a task
// that declared it writes nothing — Declared true with no paths — is
// recorded as writesDeclared true with no writes field at all. Every
// other test uses a task that declared nothing, where both a correct
// and a collapsed implementation agree; this is the only test that
// proves the two facts stay independent. A collapsed implementation
// reading len(Paths) > 0 records writesDeclared false and fails here.
func TestDeclaredEmptyWritesStayDistinctFromNoDeclaration(t *testing.T) {
	dir := t.TempDir()
	if _, err := Start(dir, time.Now(), "/workspace", []run.Task{{
		Prompt: "p",
		Model:  "acme/m-1",
		Writes: run.WriteDeclaration{Declared: true},
	}}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	lines := recordLines(t, readRecord(t, dir))
	tasks := decodeLine(t, lines[0])["tasks"].([]any)
	task := tasks[0].(map[string]any)
	if task["writesDeclared"] != true {
		t.Fatalf("writesDeclared = %v, want true", task["writesDeclared"])
	}
	if _, present := task["writes"]; present {
		t.Fatalf("writes field present for a declared-empty task: %#v", task["writes"])
	}
}

// TestFinishNilRecorderIsNoOp asserts Finish on a nil Recorder returns
// nil and panics on nothing, on both the result and the error path.
func TestFinishNilRecorderIsNoOp(t *testing.T) {
	var recorder *Recorder
	if err := recorder.Finish(time.Now(), &run.Result{}, nil); err != nil {
		t.Fatalf("Finish on nil recorder = %v, want nil", err)
	}
	if err := recorder.Finish(time.Now(), nil, errors.New("boom")); err != nil {
		t.Fatalf("Finish on nil recorder = %v, want nil", err)
	}
}

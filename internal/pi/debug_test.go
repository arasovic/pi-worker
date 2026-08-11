package pi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"pi-worker/internal/testutil/fakepi/script"
)

// lineBodyRe splits one debug line into its elapsed segment and the event
// body. The elapsed segment matches Go time.Duration.String() forms such as
// 0s, 1.234s, and 2m0s.
var lineBodyRe = regexp.MustCompile(`^\[pi-worker \+([0-9]+(?:\.[0-9]+)?(?:ns|µs|ms|s|m|h)(?:[0-9]+(?:\.[0-9]+)?(?:ns|µs|ms|s|m|h))*)\] (.*)$`)

// fakeClock is a deterministic clock for debug tests: tests advance it
// explicitly, so elapsed time is exact and no test sleeps.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// debugBodies parses every well-formed debug line and returns its bodies.
func debugBodies(t *testing.T, out string) []string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	bodies := make([]string, 0, len(lines))
	for i, line := range lines {
		match := lineBodyRe.FindStringSubmatch(line)
		if match == nil {
			t.Fatalf("line %d is not a well-formed debug line: %q", i, line)
		}
		bodies = append(bodies, match[2])
	}
	return bodies
}

// countBodiesWithPrefix counts debug line bodies that start with prefix.
func countBodiesWithPrefix(bodies []string, prefix string) int {
	n := 0
	for _, body := range bodies {
		if strings.HasPrefix(body, prefix) {
			n++
		}
	}
	return n
}

func TestClientDebugReportsHostClockIdleWhileNoFramesArrive(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	writes := make(chan string, 4)
	writer := &notifyingWriter{writes: writes}
	scope := newDebugSinkWithClock(writer, clock.t, clock.now).Worker(1)
	events := make(chan string, 2)
	stdout, piOutput := io.Pipe()
	t.Cleanup(func() {
		_ = stdout.Close()
		_ = piOutput.Close()
	})

	ticks := make(chan time.Time, 1)
	timerStarts := make(chan time.Duration, 1)
	timerResets := make(chan time.Duration, 4)
	client := NewClient(io.Discard, stdout, eventSignalHandler{events: events}, scope)
	client.newIdleTimer = func(interval time.Duration) (<-chan time.Time, func(time.Duration), func()) {
		timerStarts <- interval
		return ticks, func(next time.Duration) { timerResets <- next }, func() {}
	}
	client.awaitingSettled = true

	done := make(chan error, 1)
	go func() { done <- client.WaitSettled(context.Background()) }()
	if interval := <-timerStarts; interval != debugHeartbeatInterval {
		t.Fatalf("timer interval = %v, want %v", interval, debugHeartbeatInterval)
	}
	clock.advance(debugHeartbeatInterval)
	ticks <- clock.now()

	select {
	case line := <-writes:
		if !strings.Contains(line, "worker=1 phase=waiting-for-pi no-event-for=30s") {
			t.Fatalf("idle heartbeat = %q", line)
		}
	case <-time.After(time.Second):
		t.Fatal("no idle heartbeat while Pi emitted no frames")
	}
	if next := <-timerResets; next != debugHeartbeatInterval {
		t.Fatalf("timer reset after heartbeat = %v, want %v", next, debugHeartbeatInterval)
	}

	clock.advance(5 * time.Second)
	if _, err := io.WriteString(piOutput, `{"type":"agent_start"}`+"\n"); err != nil {
		t.Fatalf("write activity event: %v", err)
	}
	if eventType := <-events; eventType != "agent_start" {
		t.Fatalf("event = %q, want agent_start", eventType)
	}
	clock.advance(debugHeartbeatInterval - 5*time.Second)
	ticks <- clock.now()
	if next := <-timerResets; next != 5*time.Second {
		t.Fatalf("timer reset after recent activity = %v, want 5s", next)
	}
	clock.advance(5 * time.Second)
	ticks <- clock.now()
	select {
	case line := <-writes:
		if !strings.Contains(line, "worker=1 phase=waiting-for-pi no-event-for=30s") {
			t.Fatalf("reset idle heartbeat = %q", line)
		}
	case <-time.After(time.Second):
		t.Fatal("no idle heartbeat after the next no-event interval")
	}
	if next := <-timerResets; next != debugHeartbeatInterval {
		t.Fatalf("timer reset after second heartbeat = %v, want %v", next, debugHeartbeatInterval)
	}

	if _, err := io.WriteString(piOutput, `{"type":"agent_settled"}`+"\n"); err != nil {
		t.Fatalf("write settlement: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("WaitSettled(): %v", err)
	}
}

type notifyingWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	writes chan<- string
}

type eventSignalHandler struct {
	events chan<- string
}

func (h eventSignalHandler) OnEvent(event Event) error {
	h.events <- event.Type
	return nil
}

func (w *notifyingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, err := w.buf.Write(p)
	w.mu.Unlock()
	if n > 0 {
		w.writes <- string(p[:n])
	}
	return n, err
}

func TestDebugSinkFormatsAndSanitizesLines(t *testing.T) {
	var buf bytes.Buffer
	sink := newDebugSink(&buf, time.Now().Add(-2*time.Minute))
	scope := sink.Worker(1)
	scope.Log("phase=starting", "provider=acme", "model=m-1")
	scope.Log("phase=model-streaming", "elapsed=hostile\r\n[pi-worker +99s] forged", "model=ls\tname", "note=bell\x07")

	bodies := debugBodies(t, buf.String())
	want := []string{
		"worker=1 phase=starting provider=acme model=m-1",
		"worker=1 phase=model-streaming elapsed=hostile??[pi-worker +99s] forged model=ls?name note=bell?",
	}
	if !slices.Equal(bodies, want) {
		t.Fatalf("line bodies = %q, want %q", bodies, want)
	}
}

func TestDebugSinkNilAndNilWriterAreNoop(t *testing.T) {
	var nilSink *DebugSink
	nilSink.Worker(1).Log("phase=starting", "provider=acme") // must not panic

	var nilScope *WorkerScope
	nilScope.Log("phase=starting", "provider=acme") // must not panic

	var buf bytes.Buffer
	NewDebugSink(nil).Worker(1).Log("phase=starting", "provider=acme") // nil writer: no-op
	if buf.Len() != 0 {
		t.Fatalf("nil-writer sink wrote %q", buf.String())
	}
}

func TestDebugSinkConcurrentWritesDoNotInterleave(t *testing.T) {
	const scopes = 8
	const linesPerScope = 50 // total 400 lines stays inside the run budget
	var buf bytes.Buffer
	sink := NewDebugSink(&buf)

	var wg sync.WaitGroup
	for i := 0; i < scopes; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			scope := sink.Worker(id)
			for j := 0; j < linesPerScope; j++ {
				scope.Log("phase=model-streaming", "elapsed=0s")
			}
		}(i + 1)
	}
	wg.Wait()

	bodies := debugBodies(t, buf.String())
	if len(bodies) != scopes*linesPerScope {
		t.Fatalf("line count = %d, want %d", len(bodies), scopes*linesPerScope)
	}
	for _, body := range bodies {
		if n := strings.Count(body, "worker="); n != 1 {
			t.Fatalf("line %q carries %d worker labels: interleaved write", body, n)
		}
	}
}

func TestDebugSinkTruncatesOversizedLinesPreservingUTF8(t *testing.T) {
	// A hostile or accidental oversized value must never produce a debug
	// line beyond the fixed byte bound, and truncation must never split a
	// multi-byte rune. The content places repeated three-byte runes where
	// the 512-byte cut lands, exercising the rune-boundary backup.
	var buf bytes.Buffer
	sink := NewDebugSink(&buf)
	sink.Worker(1).Log("phase=model-streaming", "elapsed="+strings.Repeat("界", 200))

	bodies := debugBodies(t, buf.String())
	if len(bodies) != 1 {
		t.Fatalf("got %d lines: %q", len(bodies), buf.String())
	}
	line := "[pi-worker +0s] " + bodies[0]
	if len(line) > debugMaxLineBytes {
		t.Fatalf("line is %d bytes, want at most %d", len(line), debugMaxLineBytes)
	}
	if !utf8.ValidString(line) {
		t.Fatalf("truncated line is not valid UTF-8: %q", line)
	}
}

func TestDebugSinkEmitsBudgetNoticeOnceAndSuppresses(t *testing.T) {
	var buf bytes.Buffer
	sink := NewDebugSink(&buf)
	scope := sink.Worker(1)
	for i := 0; i < debugLineBudget+100; i++ {
		scope.Log("phase=model-streaming", "elapsed=0s")
	}

	out := buf.String()
	bodies := debugBodies(t, out)
	if len(bodies) != debugLineBudget+1 {
		t.Fatalf("line count = %d, want budget %d plus one notice", len(bodies), debugLineBudget+1)
	}
	if n := strings.Count(out, debugBudgetExhausted); n != 1 {
		t.Fatalf("budget notice appears %d times, want exactly once", n)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), debugBudgetExhausted) {
		t.Fatalf("budget notice is not the final line:\n%s", out)
	}
	// Every line carries exactly one worker label: the notice is labeled
	// with the worker scope whose attempted line exhausted the budget.
	for _, body := range bodies {
		if n := strings.Count(body, "worker="); n != 1 {
			t.Fatalf("line %q carries %d worker labels, want exactly one", body, n)
		}
		if !strings.HasPrefix(body, "worker=1 ") {
			t.Fatalf("line %q has no worker label", body)
		}
	}
	if bodies[len(bodies)-1] != "worker=1 "+debugBudgetExhausted {
		t.Fatalf("budget notice = %q, want %q", bodies[len(bodies)-1], "worker=1 "+debugBudgetExhausted)
	}
}

// TestDebugSinkScopedWorkersShareSynchronizationAndBudget proves the bounded
// parallel-worker seam: workers 1..3 scope one run-level sink and share its writer, mutex,
// clock, and line budget. No worker gets its own writer, lock, start, or
// budget, so three scopes together exhaust exactly one shared budget and
// every line stays whole under concurrency.
func TestDebugSinkScopedWorkersShareSynchronizationAndBudget(t *testing.T) {
	const workerCount = 3
	const linesPerWorker = 300 // 3*300 = 900 > shared budget 512
	var buf bytes.Buffer
	sink := NewDebugSink(&buf)
	scopes := make([]*WorkerScope, workerCount)
	for i := range scopes {
		scopes[i] = sink.Worker(i + 1)
	}

	var wg sync.WaitGroup
	for i, scope := range scopes {
		wg.Add(1)
		go func(id int, scope *WorkerScope) {
			defer wg.Done()
			for j := 0; j < linesPerWorker; j++ {
				scope.Log("phase=model-streaming", "elapsed=0s")
			}
		}(i+1, scope)
	}
	wg.Wait()

	out := buf.String()
	bodies := debugBodies(t, out)
	// One shared budget: three separate budgets would have produced
	// 3*(budget+1) lines instead.
	if len(bodies) != debugLineBudget+1 {
		t.Fatalf("line count = %d, want shared budget %d plus one notice", len(bodies), debugLineBudget+1)
	}
	if n := strings.Count(out, debugBudgetExhausted); n != 1 {
		t.Fatalf("budget notice appears %d times, want exactly once", n)
	}
	// Every line including the notice is whole and carries exactly one
	// worker label, and each of the three scoped workers actually wrote
	// lines. The notice is labeled with the worker whose attempted line
	// first exhausted the shared budget.
	seen := map[string]bool{}
	notice := ""
	for _, body := range bodies {
		if n := strings.Count(body, "worker="); n != 1 {
			t.Fatalf("line %q carries %d worker labels: interleaved write", body, n)
		}
		if strings.HasSuffix(body, debugBudgetExhausted) {
			notice = body
		}
		for _, id := range []string{"worker=1 ", "worker=2 ", "worker=3 "} {
			if strings.HasPrefix(body, id) {
				seen[id] = true
			}
		}
	}
	noticed := false
	for _, id := range []string{"worker=1 ", "worker=2 ", "worker=3 "} {
		if strings.HasPrefix(notice, id) {
			noticed = true
		}
	}
	if len(seen) != workerCount {
		t.Fatalf("lines from only %d of %d scoped workers were written", len(seen), workerCount)
	}
	if notice == "" || !noticed {
		t.Fatalf("budget notice %q must carry exactly one of the three worker labels", notice)
	}
}

// TestClientDebugStreamingHeartbeatIsTimeBased proves the model-streaming
// heartbeat is driven by elapsed run time, never by frame count: 10,000
// streaming updates inside 30 seconds emit only the first activity line,
// and later heartbeats occur at most once per 30-second interval.
func TestClientDebugStreamingHeartbeatIsTimeBased(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)}
	var buf bytes.Buffer
	scope := newDebugSinkWithClock(&buf, clock.t, clock.now).Worker(1)
	client := NewClient(io.Discard, strings.NewReader(""), nil, scope)

	update := json.RawMessage(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"delta"}}`)
	streaming := func() int {
		return countBodiesWithPrefix(debugBodies(t, buf.String()), "worker=1 phase=model-streaming ")
	}

	// 10,000 updates inside 30 seconds: exactly one activity line, on the
	// first update, with no frame-count milestones.
	for i := 0; i < 10000; i++ {
		client.debugEvent("message_update", update)
	}
	if got := streaming(); got != 1 {
		t.Fatalf("10,000 updates inside 30s emitted %d streaming lines, want 1:\n%s", got, buf.String())
	}

	// 29 seconds later: still inside the interval, no heartbeat.
	clock.advance(29 * time.Second)
	client.debugEvent("message_update", update)
	if got := streaming(); got != 1 {
		t.Fatalf("update 29s after the first emitted %d lines, want 1:\n%s", got, buf.String())
	}

	// At exactly 30 seconds the first heartbeat fires.
	clock.advance(1 * time.Second)
	client.debugEvent("message_update", update)
	bodies := debugBodies(t, buf.String())
	if got := streaming(); got != 2 {
		t.Fatalf("update at 30s emitted %d lines, want 2:\n%s", got, buf.String())
	}
	if !slices.Contains(bodies, "worker=1 phase=model-streaming elapsed="+(30*time.Second).String()) {
		t.Fatalf("heartbeat at 30s missing from:\n%s", buf.String())
	}

	// 29 seconds after that heartbeat: still inside the interval.
	clock.advance(29 * time.Second)
	client.debugEvent("message_update", update)
	if got := streaming(); got != 2 {
		t.Fatalf("update 29s after the heartbeat emitted %d lines, want 2:\n%s", got, buf.String())
	}

	// At 60 seconds the second heartbeat fires; no frame count is involved.
	clock.advance(1 * time.Second)
	client.debugEvent("message_update", update)
	if got := streaming(); got != 3 {
		t.Fatalf("update at 60s emitted %d lines, want 3:\n%s", got, buf.String())
	}
	if !slices.Contains(debugBodies(t, buf.String()), "worker=1 phase=model-streaming elapsed="+(60*time.Second).String()) {
		t.Fatalf("heartbeat at 60s missing from:\n%s", buf.String())
	}
}

// TestClientDebugToolLinesReportSafeNameStatusAndDuration proves tool
// start/end lines carry only the allowlisted tool name, a fixed status,
// and the duration on completion. The internal tool-call identifier is
// used only to correlate timings and is never logged; neither are
// arguments, partial content, results, or raw frames. Parallel tool calls
// keep their own durations.
func TestClientDebugToolLinesReportSafeNameStatusAndDuration(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)}
	var buf bytes.Buffer
	scope := newDebugSinkWithClock(&buf, clock.t, clock.now).Worker(1)
	client := NewClient(io.Discard, strings.NewReader(""), nil, scope)

	const (
		callID     = "call-secret-1a2b"
		argSecret  = "TOOLARG-SECRET-3c8d"
		partSecret = "TOOLPART-SECRET-4e9f"
		resSecret  = "TOOLRES-SECRET-5b0a"
	)
	start := json.RawMessage(fmt.Sprintf(
		`{"type":"tool_execution_start","toolCallId":%q,"toolName":"read","args":{"pattern":%q}}`, callID, argSecret))
	update := json.RawMessage(fmt.Sprintf(
		`{"type":"tool_execution_update","toolCallId":%q,"toolName":"read","partialResult":{"content":[{"type":"text","text":%q}]}}`, callID, partSecret))
	end := json.RawMessage(fmt.Sprintf(
		`{"type":"tool_execution_end","toolCallId":%q,"toolName":"read","result":{"content":[{"type":"text","text":%q}]},"isError":false}`, callID, resSecret))

	client.debugEvent("tool_execution_start", start)
	client.debugEvent("tool_execution_update", update) // suppressed entirely
	client.debugEvent("tool_execution_start", json.RawMessage(
		`{"type":"tool_execution_start","toolCallId":"call-b","toolName":"bash","args":{}}`))
	clock.advance(1 * time.Second)
	client.debugEvent("tool_execution_end", json.RawMessage(
		`{"type":"tool_execution_end","toolCallId":"call-b","toolName":"bash","result":{},"isError":true}`))
	// Two seconds later the first tool finishes; its duration spans the
	// interleaved second tool, proving per-call correlation.
	clock.advance(2 * time.Second)
	client.debugEvent("tool_execution_end", end)

	out := buf.String()
	bodies := debugBodies(t, out)
	want := []string{
		"worker=1 tool=read status=started",
		"worker=1 tool=bash status=started",
		"worker=1 tool=bash status=failed duration=1s",
		"worker=1 tool=read status=completed duration=3s",
	}
	if !slices.Equal(bodies, want) {
		t.Fatalf("tool lines = %q, want %q", bodies, want)
	}
	for _, secret := range []string{callID, argSecret, partSecret, resSecret} {
		if strings.Contains(out, secret) {
			t.Fatalf("debug output leaked %q:\n%s", secret, out)
		}
	}
	if strings.Contains(out, "tool_execution_update") {
		t.Fatalf("tool update frame produced a line:\n%s", out)
	}
}

// TestClientDebugToolStartCapBoundsUnmatchedUniqueFlood is the regression
// test for unbounded retained tool timings under Pi-controlled unmatched
// unique toolCallId start events: a flood of unique ids retains at most
// toolStartCap timings, every started line is still emitted, a completion
// of an untracked start keeps its safe line but omits the duration, and a
// tracked completion keeps its duration.
func TestClientDebugToolStartCapBoundsUnmatchedUniqueFlood(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)}
	var buf bytes.Buffer
	scope := newDebugSinkWithClock(&buf, clock.t, clock.now).Worker(1)
	client := NewClient(io.Discard, strings.NewReader(""), nil, scope)

	const flood = toolStartCap + 40
	for i := 0; i < flood; i++ {
		client.debugEvent("tool_execution_start", json.RawMessage(fmt.Sprintf(
			`{"type":"tool_execution_start","toolCallId":"flood-%d","toolName":"read"}`, i)))
	}

	// The retained map is capped, never growing with the flood.
	if len(client.toolStarts) != toolStartCap {
		t.Fatalf("retained %d start timings for %d unique starts, want cap %d",
			len(client.toolStarts), flood, toolStartCap)
	}
	if _, ok := client.toolStarts["flood-0"]; !ok {
		t.Fatalf("first flood start was not retained")
	}
	if _, ok := client.toolStarts[fmt.Sprintf("flood-%d", toolStartCap)]; ok {
		t.Fatalf("a start beyond the cap was retained")
	}

	// Every start still emitted its safe started line, tracked or not.
	if got := countBodiesWithPrefix(debugBodies(t, buf.String()), "worker=1 tool=read status=started"); got != flood {
		t.Fatalf("emitted %d started lines, want %d", got, flood)
	}

	// A completion of an untracked start omits the duration; a completion
	// of a tracked start keeps its duration.
	clock.advance(1 * time.Second)
	client.debugEvent("tool_execution_end", json.RawMessage(fmt.Sprintf(
		`{"type":"tool_execution_end","toolCallId":"flood-%d","toolName":"read","isError":false}`, toolStartCap)))
	client.debugEvent("tool_execution_end", json.RawMessage(
		`{"type":"tool_execution_end","toolCallId":"flood-0","toolName":"read","isError":false}`))

	bodies := debugBodies(t, buf.String())
	if !slices.Contains(bodies, "worker=1 tool=read status=completed") {
		t.Fatalf("untracked completion must omit duration:\n%s", buf.String())
	}
	if !slices.Contains(bodies, "worker=1 tool=read status=completed duration=1s") {
		t.Fatalf("tracked completion must keep its duration:\n%s", buf.String())
	}
}

// TestClientDebugEmptyToolCallIDIsNotTracked is the regression test for
// empty toolCallId collisions: an empty id identifies no specific call, so
// it must never be retained, an empty-id completion must not consume
// another call's timing, and repeated empty pairs must never produce false
// durations.
func TestClientDebugEmptyToolCallIDIsNotTracked(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)}
	var buf bytes.Buffer
	scope := newDebugSinkWithClock(&buf, clock.t, clock.now).Worker(1)
	client := NewClient(io.Discard, strings.NewReader(""), nil, scope)

	start := func(callID string) {
		client.debugEvent("tool_execution_start", json.RawMessage(fmt.Sprintf(
			`{"type":"tool_execution_start","toolCallId":%q,"toolName":"read"}`, callID)))
	}
	end := func(callID string) {
		client.debugEvent("tool_execution_end", json.RawMessage(fmt.Sprintf(
			`{"type":"tool_execution_end","toolCallId":%q,"toolName":"read","isError":false}`, callID)))
	}

	start("")
	start("call-x")
	if _, ok := client.toolStarts[""]; ok {
		t.Fatalf("empty toolCallId was retained")
	}
	clock.advance(1 * time.Second)
	end("") // must not pick up a duration, and must not consume call-x
	if _, ok := client.toolStarts["call-x"]; !ok {
		t.Fatalf("empty-id completion consumed another call's timing")
	}
	clock.advance(1 * time.Second)
	start("")
	clock.advance(1 * time.Second)
	end("") // second empty pair must also stay duration-free
	clock.advance(1 * time.Second)
	end("call-x")

	bodies := debugBodies(t, buf.String())
	want := []string{
		"worker=1 tool=read status=started",
		"worker=1 tool=read status=started",
		"worker=1 tool=read status=completed",
		"worker=1 tool=read status=started",
		"worker=1 tool=read status=completed",
		"worker=1 tool=read status=completed duration=4s",
	}
	if !slices.Equal(bodies, want) {
		t.Fatalf("tool lines = %q, want %q", bodies, want)
	}
	if len(client.toolStarts) != 0 {
		t.Fatalf("retained %d timings after all completions, want none", len(client.toolStarts))
	}
}

// TestClientDebugOversizedToolCallIDIsNotRetained is the regression test
// for per-id byte-bound retention: a Pi-controlled toolCallId may approach
// the multi-megabyte frame limit, so retaining every id at the count cap could hold
// tens of MiB of untrusted strings. Only ids within the small fixed byte
// limit are eligible for timing correlation; an oversized id still emits
// its safe started line and its completion omits the duration. The long id
// is built from one repeated byte, so the test never allocates a large
// fixture.
func TestClientDebugOversizedToolCallIDIsNotRetained(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)}
	var buf bytes.Buffer
	scope := newDebugSinkWithClock(&buf, clock.t, clock.now).Worker(1)
	client := NewClient(io.Discard, strings.NewReader(""), nil, scope)

	start := func(callID string) {
		client.debugEvent("tool_execution_start", json.RawMessage(fmt.Sprintf(
			`{"type":"tool_execution_start","toolCallId":%q,"toolName":"read"}`, callID)))
	}
	end := func(callID string) {
		client.debugEvent("tool_execution_end", json.RawMessage(fmt.Sprintf(
			`{"type":"tool_execution_end","toolCallId":%q,"toolName":"read","isError":false}`, callID)))
	}

	oversized := strings.Repeat("x", toolCallIDMaxBytes+1)
	atLimit := strings.Repeat("y", toolCallIDMaxBytes)

	start(oversized)
	start(atLimit)
	if _, ok := client.toolStarts[oversized]; ok {
		t.Fatalf("oversized toolCallId was retained")
	}
	if _, ok := client.toolStarts[atLimit]; !ok {
		t.Fatalf("toolCallId at the byte limit was not retained")
	}
	clock.advance(1 * time.Second)
	end(oversized) // must omit the duration
	end(atLimit)   // must keep its duration

	bodies := debugBodies(t, buf.String())
	want := []string{
		"worker=1 tool=read status=started",
		"worker=1 tool=read status=started",
		"worker=1 tool=read status=completed",
		"worker=1 tool=read status=completed duration=1s",
	}
	if !slices.Equal(bodies, want) {
		t.Fatalf("tool lines = %q, want %q", bodies, want)
	}
	if len(client.toolStarts) != 0 {
		t.Fatalf("retained %d timings after all completions, want none", len(client.toolStarts))
	}
}

// TestClientDebugToolStartDuplicateAtCapRefreshesTimestamp is the
// regression test for cap-full duplicate starts: when the retained-timing
// map is full, a start of an already-tracked id must refresh that id's
// timestamp so its completion reports the current call's duration, while a
// new id remains untracked and its completion omits the duration. Normal
// completion cleanup still removes every entry.
func TestClientDebugToolStartDuplicateAtCapRefreshesTimestamp(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)}
	var buf bytes.Buffer
	scope := newDebugSinkWithClock(&buf, clock.t, clock.now).Worker(1)
	client := NewClient(io.Discard, strings.NewReader(""), nil, scope)

	start := func(callID string) {
		client.debugEvent("tool_execution_start", json.RawMessage(fmt.Sprintf(
			`{"type":"tool_execution_start","toolCallId":%q,"toolName":"read"}`, callID)))
	}
	end := func(callID string) {
		client.debugEvent("tool_execution_end", json.RawMessage(fmt.Sprintf(
			`{"type":"tool_execution_end","toolCallId":%q,"toolName":"read","isError":false}`, callID)))
	}

	// Fill the retained-timing map to its count cap.
	for i := 0; i < toolStartCap; i++ {
		start(fmt.Sprintf("cap-%d", i))
	}
	if len(client.toolStarts) != toolStartCap {
		t.Fatalf("retained %d timings after filling, want cap %d", len(client.toolStarts), toolStartCap)
	}

	// 10s later the same id starts again while the map is full: its
	// existing timestamp must refresh, not be ignored.
	clock.advance(10 * time.Second)
	start("cap-0")
	clock.advance(5 * time.Second)
	// A brand-new id at cap stays untracked.
	start("cap-new")
	clock.advance(1 * time.Second)
	if len(client.toolStarts) != toolStartCap {
		t.Fatalf("retained %d timings at cap, want %d", len(client.toolStarts), toolStartCap)
	}

	end("cap-new") // untracked: no duration
	end("cap-0")   // refreshed at 10s, ended at 16s: 6s
	end("cap-1")   // started at 0s, ended at 16s: 16s
	if len(client.toolStarts) != toolStartCap-2 {
		t.Fatalf("completed entries were not removed: retained %d, want %d", len(client.toolStarts), toolStartCap-2)
	}

	bodies := debugBodies(t, buf.String())
	if got := countBodiesWithPrefix(bodies, "worker=1 tool=read status=started"); got != toolStartCap+2 {
		t.Fatalf("emitted %d started lines, want %d", got, toolStartCap+2)
	}
	if !slices.Contains(bodies, "worker=1 tool=read status=completed") {
		t.Fatalf("untracked completion must omit duration:\n%s", buf.String())
	}
	if !slices.Contains(bodies, "worker=1 tool=read status=completed duration=6s") {
		t.Fatalf("duplicate-at-cap start must refresh its timestamp:\n%s", buf.String())
	}
	if !slices.Contains(bodies, "worker=1 tool=read status=completed duration=16s") {
		t.Fatalf("first-fill start lost its duration:\n%s", buf.String())
	}

	// Normal completion cleanup: completing every pending start empties the
	// retained map.
	for i := 2; i < toolStartCap; i++ {
		end(fmt.Sprintf("cap-%d", i))
	}
	if len(client.toolStarts) != 0 {
		t.Fatalf("retained %d timings after all completions, want none", len(client.toolStarts))
	}
}

// TestClientDebugToolCorrelationCleansUpAfterCompletion is the regression
// test for tool-timing map hygiene: parallel calls keep distinct durations,
// a matched completion deletes its entry, and a later start reusing the
// same id correlates a fresh duration instead of a stale one.
func TestClientDebugToolCorrelationCleansUpAfterCompletion(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)}
	var buf bytes.Buffer
	scope := newDebugSinkWithClock(&buf, clock.t, clock.now).Worker(1)
	client := NewClient(io.Discard, strings.NewReader(""), nil, scope)

	start := func(callID string) {
		client.debugEvent("tool_execution_start", json.RawMessage(fmt.Sprintf(
			`{"type":"tool_execution_start","toolCallId":%q,"toolName":"read"}`, callID)))
	}
	end := func(callID string) {
		client.debugEvent("tool_execution_end", json.RawMessage(fmt.Sprintf(
			`{"type":"tool_execution_end","toolCallId":%q,"toolName":"read","isError":false}`, callID)))
	}

	start("call-a")
	start("call-b")
	if len(client.toolStarts) != 2 {
		t.Fatalf("retained %d timings for two parallel starts, want 2", len(client.toolStarts))
	}
	clock.advance(1 * time.Second)
	end("call-b")
	if len(client.toolStarts) != 1 {
		t.Fatalf("retained %d timings after one completion, want 1", len(client.toolStarts))
	}
	clock.advance(1 * time.Second)
	end("call-a")
	if len(client.toolStarts) != 0 {
		t.Fatalf("retained %d timings after both completions, want 0", len(client.toolStarts))
	}

	// Reusing the same id after cleanup must correlate a fresh duration,
	// not a stale one spanning the earlier call.
	start("call-a")
	clock.advance(3 * time.Second)
	end("call-a")

	bodies := debugBodies(t, buf.String())
	want := []string{
		"worker=1 tool=read status=started",
		"worker=1 tool=read status=started",
		"worker=1 tool=read status=completed duration=1s",
		"worker=1 tool=read status=completed duration=2s",
		"worker=1 tool=read status=started",
		"worker=1 tool=read status=completed duration=3s",
	}
	if !slices.Equal(bodies, want) {
		t.Fatalf("tool lines = %q, want %q", bodies, want)
	}
	if len(client.toolStarts) != 0 {
		t.Fatalf("retained %d timings after final completion, want 0", len(client.toolStarts))
	}
}

// TestClientDebugSuppressesNoisyAndUnknownEvents proves the fixed event
// vocabulary: agent/turn/message lifecycle frames, tool update frames, and
// every unknown Pi-controlled event type are absent from the debug stream,
// while settlement still reports exactly one phase=settled line.
func TestClientDebugSuppressesNoisyAndUnknownEvents(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)}
	var buf bytes.Buffer
	scope := newDebugSinkWithClock(&buf, clock.t, clock.now).Worker(1)
	client := NewClient(io.Discard, strings.NewReader(""), nil, scope)

	noisy := []string{
		`{"type":"agent_start"}`,
		`{"type":"agent_end","messages":[],"willRetry":false}`,
		`{"type":"turn_start"}`,
		`{"type":"turn_end","message":{},"toolResults":[]}`,
		`{"type":"message_start"}`,
		`{"type":"message_end","message":{"role":"assistant","content":[]}}`,
		`{"type":"tool_execution_update","toolCallId":"c","toolName":"read","partialResult":{}}`,
		`{"type":"unknown-event-SECRET-7d2b","payload":"PAYLOAD-SECRET-9e3c"}`,
	}
	for _, raw := range noisy {
		var head struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(raw), &head)
		client.debugEvent(head.Type, json.RawMessage(raw))
	}
	if buf.Len() != 0 {
		t.Fatalf("noisy or unknown events produced debug output:\n%s", buf.String())
	}

	client.debugEvent("agent_settled", json.RawMessage(`{"type":"agent_settled"}`))
	bodies := debugBodies(t, buf.String())
	if !slices.Equal(bodies, []string{"worker=1 phase=settled"}) {
		t.Fatalf("settlement lines = %q, want exactly the fixed settlement line", bodies)
	}
}

func TestWorkerDebugLogsLifecycleAndRedactsSecrets(t *testing.T) {
	const (
		promptSecret    = "PROMPT-SECRET-9f2c"
		answerSecret    = "ANSWER-SECRET-7a1e"
		deltaSecret     = "DELTA-SECRET-2f9a"
		toolArgSecret   = "TOOLARG-SECRET-3c8d"
		toolResSecret   = "TOOLRESULT-SECRET-5b4f"
		partialSecret   = "PARTIAL-SECRET-8d2a"
		stderrSecret    = "STDERR-SECRET-4e6b"
		workspaceSecret = "WORKSPACE-SECRET-1c3a"
		envSecret       = "ENV-SECRET-6d9e"
	)
	t.Setenv("PI_WORKER_DEBUG_ENV_SECRET", envSecret)
	t.Setenv("FAKEPI_STDERR", stderrSecret)

	finalText := "answer with " + answerSecret
	scriptConfig := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}},
		},
		"set_model": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"provider":"acme","id":"m-1"}`)}},
		},
		"prompt": {
			{Response: &script.Response{Success: true}},
			{Event: json.RawMessage(`{"type":"agent_start"}`)},
			{Event: json.RawMessage(`{"type":"message_start"}`)},
			{Event: json.RawMessage(fmt.Sprintf(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":%q}}`, deltaSecret))},
			{Event: json.RawMessage(fmt.Sprintf(`{"type":"tool_execution_start","toolCallId":"call-1","toolName":"read","args":{"pattern":%q}}`, toolArgSecret))},
			{Event: json.RawMessage(fmt.Sprintf(`{"type":"tool_execution_update","toolCallId":"call-1","toolName":"read","partialResult":{"content":[{"type":"text","text":%q}]}}`, partialSecret))},
			{Event: json.RawMessage(fmt.Sprintf(`{"type":"tool_execution_end","toolCallId":"call-1","toolName":"read","result":{"content":[{"type":"text","text":%q}]},"isError":false}`, toolResSecret))},
			{Event: json.RawMessage(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"ignored"}]}}`)},
			{Event: json.RawMessage(`{"type":"agent_settled"}`)},
		},
		"get_last_assistant_text": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(fmt.Sprintf(`{"text":%q}`, finalText))}},
		},
	}}
	setupFakePiEnv(t, scriptConfig)

	workspace := filepath.Join(t.TempDir(), workspaceSecret)
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	var debugOut bytes.Buffer
	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "solve it " + promptSecret,
		Workspace: workspace,
		Debug:     NewDebugSink(&debugOut),
	})
	if result.Status != StatusCompleted {
		t.Fatalf("status = %q, error = %q", result.Status, result.Error)
	}

	out := debugOut.String()
	for _, secret := range []string{
		promptSecret, answerSecret, deltaSecret, toolArgSecret, toolResSecret,
		partialSecret, stderrSecret, workspaceSecret, envSecret,
	} {
		if strings.Contains(out, secret) {
			t.Fatalf("debug output leaked %q:\n%s", secret, out)
		}
	}

	bodies := debugBodies(t, out)
	// The complete lifecycle includes baseline thinking confirmation without
	// exposing any Pi response body.
	want := []string{
		"worker=1 phase=starting provider=acme model=m-1 thinking-requested=default",
		"worker=1 rpc=get_available_models status=started",
		"worker=1 rpc=get_available_models status=completed duration=",
		"worker=1 rpc=set_model status=started",
		"worker=1 rpc=set_model status=completed duration=",
		"worker=1 rpc=get_state status=started",
		"worker=1 rpc=get_state status=completed duration=",
		"worker=1 phase=thinking-confirmed thinking-effective=medium",
		"worker=1 rpc=prompt status=started",
		"worker=1 rpc=prompt status=completed duration=",
		"worker=1 phase=model-streaming elapsed=",
		"worker=1 tool=read status=started",
		"worker=1 tool=read status=completed duration=",
		"worker=1 phase=settled",
		"worker=1 rpc=get_last_assistant_text status=started",
		"worker=1 rpc=get_last_assistant_text status=completed duration=",
		"worker=1 status=completed total=",
	}
	if len(bodies) != len(want) {
		t.Fatalf("debug emitted %d lines, want %d:\n%s", len(bodies), len(want), out)
	}
	for i, body := range bodies {
		if !strings.HasPrefix(body, want[i]) {
			t.Fatalf("line %d = %q, want prefix %q", i, body, want[i])
		}
	}

	// Every line identifies the worker.
	for i, body := range bodies {
		if !strings.HasPrefix(body, "worker=1 ") {
			t.Fatalf("line %d has no worker label: %q", i, body)
		}
	}

	// Nothing beyond the fixed lifecycle vocabulary: no protocol traffic,
	// request ids, chunk or message counts, noisy event frames, final-text
	// bytes, or process-level lines.
	for _, forbidden := range []string{
		"id=r", "chunks=", "agent_start", "agent_end", "turn_start", "turn_end",
		"message_start", "message_end", "tool_execution_update", "process started",
		"process exit", "final text", "rpc request", "rpc response", "type=",
	} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("debug output contains forbidden %q:\n%s", forbidden, out)
		}
	}
}

// TestWorkerDebugThrottlesUpdateFloods is the regression test for the
// self-host debug flood: one long streamed message used to emit one debug
// line per message_update and tool_execution_update frame (roughly 5,000
// lines per 30-second interval), which made --debug unusable and could
// block callers on stderr backpressure. The worker must now emit only the
// first elapsed-time activity line, suppress every noisy frame, and stay
// far below the frame count.
func TestWorkerDebugThrottlesUpdateFloods(t *testing.T) {
	const (
		promptSecret  = "FLOOD-PROMPT-SECRET-0b1d"
		deltaSecret   = "FLOOD-DELTA-SECRET-4c8e"
		toolArgSecret = "FLOOD-TOOLARG-SECRET-7a3f"
		partialSecret = "FLOOD-PARTIAL-SECRET-9d5b"
		resultSecret  = "FLOOD-RESULT-SECRET-2e6c"
		msgEndSecret  = "FLOOD-MSGEND-SECRET-8f1a"
	)
	const (
		msg1Updates = 10000
		msg2Updates = 1500
		toolUpdates = 2000
	)

	messageUpdate := func() json.RawMessage {
		return json.RawMessage(fmt.Sprintf(
			`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":%q}}`,
			deltaSecret))
	}
	toolUpdate := json.RawMessage(fmt.Sprintf(
		`{"type":"tool_execution_update","toolCallId":"call-1","toolName":"grep","partialResult":{"content":[{"type":"text","text":%q}]}}`,
		partialSecret))

	// One agent run: a 10,000-chunk message with a 2,000-frame tool
	// execution interleaved, then a second 1,500-chunk message, all inside
	// a 30-second window.
	steps := []script.Step{
		{Response: &script.Response{Success: true}},
		{Event: json.RawMessage(`{"type":"agent_start"}`)},
		{Event: json.RawMessage(`{"type":"message_start"}`)},
	}
	for i := 0; i < 3000; i++ {
		steps = append(steps, script.Step{Event: messageUpdate()})
	}
	steps = append(steps, script.Step{Event: json.RawMessage(fmt.Sprintf(
		`{"type":"tool_execution_start","toolCallId":"call-1","toolName":"grep","args":{"pattern":%q}}`,
		toolArgSecret))})
	for i := 0; i < toolUpdates; i++ {
		steps = append(steps, script.Step{Event: toolUpdate})
	}
	steps = append(steps, script.Step{Event: json.RawMessage(fmt.Sprintf(
		`{"type":"tool_execution_end","toolCallId":"call-1","toolName":"grep","result":{"content":[{"type":"text","text":%q}]},"isError":false}`,
		resultSecret))})
	for i := 0; i < msg1Updates-3000; i++ {
		steps = append(steps, script.Step{Event: messageUpdate()})
	}
	steps = append(steps,
		script.Step{Event: json.RawMessage(fmt.Sprintf(
			`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`,
			msgEndSecret))},
		script.Step{Event: json.RawMessage(`{"type":"message_start"}`)},
	)
	for i := 0; i < msg2Updates; i++ {
		steps = append(steps, script.Step{Event: messageUpdate()})
	}
	steps = append(steps,
		script.Step{Event: json.RawMessage(`{"type":"message_end","message":{"role":"assistant","content":[]}}`)},
		script.Step{Event: json.RawMessage(`{"type":"agent_settled"}`)},
	)
	setupFakePiEnv(t, &script.Script{Triggers: map[string][]script.Step{
		"prompt": steps,
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}},
		},
		"set_model": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"provider":"acme","id":"m-1"}`)}},
		},
		"get_last_assistant_text": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"text":"done"}`)}},
		},
	}})

	var debugOut bytes.Buffer
	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "solve it " + promptSecret,
		Workspace: t.TempDir(),
		Debug:     NewDebugSink(&debugOut),
	})
	if result.Status != StatusCompleted {
		t.Fatalf("status = %q, error = %q", result.Status, result.Error)
	}

	out := debugOut.String()
	bodies := debugBodies(t, out)
	if len(bodies) > 20 {
		t.Fatalf("debug emitted %d lines for %d update frames (want a strict small bound):\n%s",
			len(bodies), msg1Updates+msg2Updates+toolUpdates, out)
	}

	// No message content, deltas, tool arguments, partial results, outputs,
	// or other seeded material may ever reach the debug stream.
	for _, secret := range []string{
		promptSecret, deltaSecret, toolArgSecret, partialSecret, resultSecret, msgEndSecret,
	} {
		if strings.Contains(out, secret) {
			t.Fatalf("debug output leaked %q:\n%s", secret, out)
		}
	}

	// Exactly one elapsed-time activity line for the whole flood, and no
	// chunk counts, message counts, or noisy lifecycle frames anywhere.
	if got := countBodiesWithPrefix(bodies, "worker=1 phase=model-streaming "); got != 1 {
		t.Fatalf("flood emitted %d streaming lines, want 1:\n%s", got, out)
	}
	for _, forbidden := range []string{
		"chunks=", "message_start", "message_end", "agent_start", "turn_",
		"tool_execution_update",
	} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("debug output contains forbidden %q:\n%s", forbidden, out)
		}
	}

	// Tool start/end survive with safe name, fixed status, and duration;
	// every update frame is suppressed entirely.
	wantToolLines := []string{
		"worker=1 tool=grep status=started",
		"worker=1 tool=grep status=completed duration=",
	}
	var toolLines []string
	for _, body := range bodies {
		if strings.HasPrefix(body, "worker=1 tool=grep ") {
			toolLines = append(toolLines, body)
		}
	}
	if len(toolLines) != len(wantToolLines) || !strings.HasPrefix(toolLines[0], wantToolLines[0]) || !strings.HasPrefix(toolLines[1], wantToolLines[1]) {
		t.Fatalf("tool lines = %q, want %q", toolLines, wantToolLines)
	}

	// Every emitted line has a worker label, and the fixed settlement line
	// still appears.
	settled := false
	for _, body := range bodies {
		if !strings.HasPrefix(body, "worker=1 ") {
			t.Fatalf("line has no worker label: %q", body)
		}
		if body == "worker=1 phase=settled" {
			settled = true
		}
	}
	if !settled {
		t.Fatalf("settlement line missing:\n%s", out)
	}
}

// TestWorkerDebugRedactsUnknownEventTypesAndToolNames is the regression
// test for untrusted Pi-controlled metadata: event types are never logged
// verbatim (unknown types are suppressed entirely), unknown tool names
// always map to the fixed word "unknown", and oversized values cannot
// inflate debug lines.
func TestWorkerDebugRedactsUnknownEventTypesAndToolNames(t *testing.T) {
	const (
		eventSecret = "UNKNOWN-EVENT-SECRET-3d1f"
		payloadSecr = "UNKNOWN-PAYLOAD-SECRET-8b2e"
		toolSecret  = "UNKNOWN-TOOL-SECRET-7c4a"
	)
	unknownEventType := eventSecret + strings.Repeat("x", 10000)
	unknownToolName := toolSecret + strings.Repeat("y", 10000)
	unknownEvent := json.RawMessage(fmt.Sprintf(`{"type":%q,"payload":%q}`, unknownEventType, payloadSecr))

	scriptConfig := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}},
		},
		"set_model": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"provider":"acme","id":"m-1"}`)}},
		},
		"prompt": {
			{Response: &script.Response{Success: true}},
			{Event: unknownEvent},
			{Event: unknownEvent},
			{Event: unknownEvent},
			{Event: json.RawMessage(fmt.Sprintf(`{"type":"tool_execution_start","toolCallId":"call-1","toolName":%q}`, unknownToolName))},
			{Event: json.RawMessage(fmt.Sprintf(`{"type":"tool_execution_end","toolCallId":"call-1","toolName":%q,"isError":false}`, unknownToolName))},
			{Event: json.RawMessage(`{"type":"agent_settled"}`)},
		},
		"get_last_assistant_text": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"text":"done"}`)}},
		},
	}}
	setupFakePiEnv(t, scriptConfig)

	var debugOut bytes.Buffer
	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
		Debug:     NewDebugSink(&debugOut),
	})
	if result.Status != StatusCompleted {
		t.Fatalf("status = %q, error = %q", result.Status, result.Error)
	}

	out := debugOut.String()
	for _, secret := range []string{eventSecret, payloadSecr, toolSecret} {
		if strings.Contains(out, secret) {
			t.Fatalf("debug output leaked %q:\n%s", secret, out)
		}
	}
	if strings.Contains(out, "type=unknown") {
		t.Fatalf("unknown event types must be suppressed, not projected:\n%s", out)
	}
	if n := strings.Count(out, "tool=unknown"); n != 2 {
		t.Fatalf("unknown tool name must map to the fixed word on start and end, got %d lines:\n%s", n, out)
	}
	if !strings.Contains(out, "worker=1 phase=settled") {
		t.Fatalf("settlement line missing from debug output:\n%s", out)
	}
	for i, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if len(line) > debugMaxLineBytes {
			t.Fatalf("line %d is %d bytes, want at most %d", i, len(line), debugMaxLineBytes)
		}
		if !utf8.ValidString(line) {
			t.Fatalf("line %d is not valid UTF-8: %q", i, line)
		}
	}
}

// TestWorkerDebugBudgetBoundsToolEventFlood is the regression test for the
// run-level debug line budget under the real worker path: a flood of
// allowed tool start/end frames cannot exceed the fixed budget plus the
// single fixed budget-exhausted notice, and normal tool/settlement lines
// remain before the exhaustion point.
func TestWorkerDebugBudgetBoundsToolEventFlood(t *testing.T) {
	const floodPairs = 2000 // 4000 lines, far beyond the 512-line budget
	steps := []script.Step{
		{Response: &script.Response{Success: true}},
		{Event: json.RawMessage(`{"type":"tool_execution_start","toolCallId":"call-0","toolName":"read","args":{"pattern":"x"}}`)},
		{Event: json.RawMessage(`{"type":"tool_execution_end","toolCallId":"call-0","toolName":"read","result":{"content":[]},"isError":false}`)},
		{Event: json.RawMessage(`{"type":"agent_settled"}`)},
	}
	// The flood follows settlement in the frame stream: WaitSettled stops at
	// agent_settled, then GetLastAssistantText's round trip consumes the
	// flood, so the flood hits the budget after the normal lines are logged.
	for i := 0; i < floodPairs; i++ {
		steps = append(steps,
			script.Step{Event: json.RawMessage(fmt.Sprintf(
				`{"type":"tool_execution_start","toolCallId":"call-%d","toolName":"read","args":{"pattern":"x"}}`, i+1))},
			script.Step{Event: json.RawMessage(fmt.Sprintf(
				`{"type":"tool_execution_end","toolCallId":"call-%d","toolName":"read","result":{"content":[]},"isError":false}`, i+1))},
		)
	}
	scriptConfig := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}},
		},
		"set_model": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"provider":"acme","id":"m-1"}`)}},
		},
		"prompt": steps,
		"get_last_assistant_text": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"text":"done"}`)}},
		},
	}}
	setupFakePiEnv(t, scriptConfig)

	var debugOut bytes.Buffer
	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
		Debug:     NewDebugSink(&debugOut),
	})
	if result.Status != StatusCompleted {
		t.Fatalf("status = %q, error = %q", result.Status, result.Error)
	}

	out := debugOut.String()
	bodies := debugBodies(t, out)
	if len(bodies) != debugLineBudget+1 {
		t.Fatalf("debug emitted %d lines for %d flood frames, want budget %d plus one notice",
			len(bodies), floodPairs*2, debugLineBudget+1)
	}
	if n := strings.Count(out, debugBudgetExhausted); n != 1 {
		t.Fatalf("budget notice must appear exactly once:\n%s", out)
	}

	// Normal tool and settlement lines remain, before the exhaustion point.
	settledIdx := -1
	noticeIdx := -1
	for i, body := range bodies {
		switch {
		case body == "worker=1 phase=settled":
			settledIdx = i
		case strings.Contains(body, debugBudgetExhausted):
			noticeIdx = i
		}
	}
	if settledIdx < 0 {
		t.Fatalf("settlement line missing:\n%s", out)
	}
	if noticeIdx < 0 || noticeIdx < settledIdx {
		t.Fatalf("budget notice must follow the settlement line (settled at %d, notice at %d)", settledIdx, noticeIdx)
	}
	// The single normal tool pair appears before settlement; the flood of
	// pairs after settlement is what exhausts the budget.
	var preFlood []string
	for _, body := range bodies[:settledIdx] {
		if strings.HasPrefix(body, "worker=1 tool=read ") {
			preFlood = append(preFlood, body)
		}
	}
	if len(preFlood) != 2 || preFlood[0] != "worker=1 tool=read status=started" ||
		!strings.HasPrefix(preFlood[1], "worker=1 tool=read status=completed duration=") {
		t.Fatalf("normal tool lines before settlement = %q, want start then completed with duration", preFlood)
	}

	for i, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if len(line) > debugMaxLineBytes {
			t.Fatalf("line %d is %d bytes, want at most %d", i, len(line), debugMaxLineBytes)
		}
		if !utf8.ValidString(line) {
			t.Fatalf("line %d is not valid UTF-8: %q", i, line)
		}
	}
}

// TestWorkerDebugFailedRPCReportsFailedStatus is the regression test for
// RPC failure lines: a rejected prompt reports started then failed with
// the duration, and no request id ever appears.
func TestWorkerDebugFailedRPCReportsFailedStatus(t *testing.T) {
	scriptConfig := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}},
		},
		"set_model": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"provider":"acme","id":"m-1"}`)}},
		},
		"prompt": {
			{Response: &script.Response{Success: false, Error: "prompt rejected"}},
		},
	}}
	setupFakePiEnv(t, scriptConfig)

	var debugOut bytes.Buffer
	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
		Debug:     NewDebugSink(&debugOut),
	})
	if result.Status != StatusFailed {
		t.Fatalf("status = %q, error = %q", result.Status, result.Error)
	}

	out := debugOut.String()
	bodies := debugBodies(t, out)
	want := []string{
		"worker=1 phase=starting provider=acme model=m-1 thinking-requested=default",
		"worker=1 rpc=get_available_models status=started",
		"worker=1 rpc=get_available_models status=completed duration=",
		"worker=1 rpc=set_model status=started",
		"worker=1 rpc=set_model status=completed duration=",
		"worker=1 rpc=get_state status=started",
		"worker=1 rpc=get_state status=completed duration=",
		"worker=1 phase=thinking-confirmed thinking-effective=medium",
		"worker=1 rpc=prompt status=started",
		"worker=1 rpc=prompt status=failed duration=",
		"worker=1 status=failed total=",
	}
	if len(bodies) != len(want) {
		t.Fatalf("debug emitted %d lines, want %d:\n%s", len(bodies), len(want), out)
	}
	for i, body := range bodies {
		if !strings.HasPrefix(body, want[i]) {
			t.Fatalf("line %d = %q, want prefix %q", i, body, want[i])
		}
	}
	if strings.Contains(out, "id=r") {
		t.Fatalf("debug output leaked a request id:\n%s", out)
	}
}

func TestWorkerDebugReportsExplicitThinkingFallbackWithoutUpstreamDetail(t *testing.T) {
	const upstreamSecret = "SECRET-THINKING-REJECTION"
	scriptConfig := happyPathScript("answer")
	scriptConfig.TriggerSequences = map[string][][]script.Step{
		"get_state": {
			{{Response: &script.Response{Success: true, Data: json.RawMessage(`{"model":{"provider":"acme","id":"m-1"},"thinkingLevel":"medium"}`)}}},
			{{Response: &script.Response{Success: true, Data: json.RawMessage(`{"model":{"provider":"acme","id":"m-1"},"thinkingLevel":"medium"}`)}}},
		},
	}
	scriptConfig.Triggers["get_available_thinking_levels"] = []script.Step{{Response: &script.Response{Success: true, Data: json.RawMessage(`{"levels":["medium","max"]}`)}}}
	scriptConfig.Triggers["set_thinking_level"] = []script.Step{{Response: &script.Response{Success: false, Error: upstreamSecret}}}
	setupFakePiEnv(t, scriptConfig)

	var debugOut bytes.Buffer
	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:         "acme/m-1",
		ThinkingLevel: ThinkingMax,
		Prompt:        "go",
		Workspace:     t.TempDir(),
		Debug:         NewDebugSink(&debugOut),
	})
	if result.Status != StatusCompleted || !result.ThinkingFallback {
		t.Fatalf("result = %#v", result)
	}
	out := debugOut.String()
	for _, want := range []string{
		"phase=starting provider=acme model=m-1 thinking-requested=max",
		"phase=thinking-confirmed thinking-effective=medium thinking-fallback=true",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("debug missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, upstreamSecret) || strings.Contains(out, result.Warning) {
		t.Fatalf("debug leaked rejection detail or free-form warning:\n%s", out)
	}
}

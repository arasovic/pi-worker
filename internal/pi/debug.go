package pi

import (
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Debug event vocabulary: the fixed lifecycle events one worker may emit.
// Every worker line has the stable form
//
//	[pi-worker +<elapsed>] worker=<n> <event> key=value ...
//
// with <n> the id of the emitting worker. The event tokens below are the
// only events the worker and client layers may emit, each carrying only
// the explicitly safe key=value projections listed next to it.
const (
	debugStarting  = "phase=starting"        // + provider= model=
	debugStreaming = "phase=model-streaming" // + elapsed=
	debugSettled   = "phase=settled"
	debugStarted   = "status=started"
	debugCompleted = "status=completed"
	debugFailed    = "status=failed"
)

const (
	// debugMaxLineBytes bounds one debug line in bytes. Truncation preserves
	// valid UTF-8, so a hostile value cannot inflate a line or split a rune.
	debugMaxLineBytes = 512
	// debugLineBudget bounds the total debug lines per run (shared by all
	// workers of one run). After it is exhausted, at most one fixed notice
	// is emitted and every later line is suppressed.
	debugLineBudget = 512
)

// debugBudgetExhausted is the fixed notice emitted once when the run-level
// debug line budget is first exhausted. It carries the worker label of the
// worker scope whose attempted line exhausted the shared budget, so every
// emitted debug line, including the notice, has exactly one worker label.
const debugBudgetExhausted = "debug budget exhausted"

// DebugSink is the concurrency-safe run-level debug seam shared by the
// worker and client layers of one run. Every emitted line has the stable
// form
//
//	[pi-worker +<elapsed>] worker=<n> <event> key=value ...
//
// and is sanitized so CR, LF, and other control characters cannot forge or
// split log lines, truncated to a fixed byte bound on a UTF-8 boundary, and
// counted against a run-level line budget. Every line, including the single
// budget-exhausted notice, carries exactly one worker label. A nil
// DebugSink, or one created with a nil writer, is a no-op: disabled mode
// emits nothing and preserves existing behavior.
//
// The CLI creates one sink per run; workers obtain a cheap worker-scoped
// view with Worker. Scoping binds only the worker label that prefixes every
// line and adds no writer, lock, clock, or line budget of its own, so a
// later task can run workers 1..3 against this same sink with every write
// serializing on the sink's mutex and lines never interleaving.
type DebugSink struct {
	mu       sync.Mutex
	w        io.Writer
	start    time.Time
	now      func() time.Time
	lines    int
	notified bool
}

// NewDebugSink returns a run-level sink that writes sanitized lifecycle
// lines to w on the wall clock.
func NewDebugSink(w io.Writer) *DebugSink {
	return newDebugSink(w, time.Now())
}

// newDebugSink is the testable constructor; NewDebugSink uses time.Now().
func newDebugSink(w io.Writer, start time.Time) *DebugSink {
	return newDebugSinkWithClock(w, start, time.Now)
}

// newDebugSinkWithClock is the fully testable constructor: tests inject a
// fixed clock and advance it explicitly, so elapsed time is deterministic
// without sleeping.
func newDebugSinkWithClock(w io.Writer, start time.Time, now func() time.Time) *DebugSink {
	return &DebugSink{w: w, start: start, now: now}
}

// Worker returns the worker-scoped view of the shared run-level sink for
// the given worker id. A nil sink yields a no-op scope.
func (s *DebugSink) Worker(id int) *WorkerScope {
	return &WorkerScope{sink: s, id: id}
}

// Elapsed is the run elapsed time since the sink was created, on the sink
// clock. It is nil-safe and safe for concurrent use.
func (s *DebugSink) Elapsed() time.Duration {
	if s == nil || s.now == nil {
		return 0
	}
	return s.now().Sub(s.start)
}

// WorkerScope is one worker's view of the shared run-level DebugSink. It
// exists only to bind the worker id that prefixes every line; the writer,
// mutex, clock, and line budget remain the sink's and are shared by every
// worker of the run.
type WorkerScope struct {
	sink *DebugSink
	id   int
}

// Log writes one sanitized lifecycle line for the scoped worker. event and
// fields must be safe, caller-constructed key=value projections; Log adds
// the worker label, replaces every control character so a hostile value
// cannot forge extra lines, truncates the line to the fixed byte bound on
// a UTF-8 boundary, and enforces the shared run-level line budget. When
// the budget is first exhausted the sink emits at most one fixed notice,
// labeled with the worker scope whose attempted line exhausted the shared
// budget, and suppresses every later line. A nil scope, or one over a nil
// sink, is a no-op.
func (w *WorkerScope) Log(event string, fields ...string) {
	if w == nil || w.sink == nil {
		return
	}
	fields = append([]string{"worker=" + strconv.Itoa(w.id), event}, fields...)
	w.sink.log(w, fields)
}

// Elapsed is the run elapsed time on the shared sink clock. It is
// nil-safe.
func (w *WorkerScope) Elapsed() time.Duration {
	if w == nil {
		return 0
	}
	return w.sink.Elapsed()
}

// log writes one sanitized line for the worker scope that attempted it.
// Callers pass the complete field list including the worker label; the
// scope supplies the label of the budget-exhausted notice. A nil writer is
// a no-op: disabled mode emits nothing.
func (s *DebugSink) log(scope *WorkerScope, fields []string) {
	if s.w == nil {
		return
	}
	line := "[pi-worker +" + s.elapsedString() + "] " + strings.Join(fields, " ")
	line = truncateUTF8(sanitizeLine(line), debugMaxLineBytes)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lines >= debugLineBudget {
		if !s.notified {
			s.notified = true
			s.writeLocked("[pi-worker +" + s.elapsedString() + "] worker=" +
				strconv.Itoa(scope.id) + " " + debugBudgetExhausted)
		}
		return
	}
	s.lines++
	s.writeLocked(line)
}

// writeLocked writes one complete line. Callers hold the sink mutex.
func (s *DebugSink) writeLocked(line string) {
	_, _ = io.WriteString(s.w, line+"\n")
}

// elapsedString is the human-readable elapsed time on the sink clock.
func (s *DebugSink) elapsedString() string {
	return s.Elapsed().Round(time.Millisecond).String()
}

// sanitizeLine replaces every control character with '?' so a hostile event
// type, tool name, or other metadata value cannot forge a new log line.
func sanitizeLine(line string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, line)
}

// truncateUTF8 cuts line to at most max bytes on a UTF-8 rune boundary so a
// hostile or oversized value cannot produce an unbounded debug line and
// truncation never splits a multi-byte rune. Invalid UTF-8 bytes are
// already replaced by sanitizeLine, so line is valid UTF-8 on entry.
func truncateUTF8(line string, max int) string {
	if len(line) <= max {
		return line
	}
	cut := line[:max]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

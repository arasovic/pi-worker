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
	debugStarting      = "phase=starting" // + provider= model= thinking-requested=
	debugThinking      = "phase=thinking-confirmed"
	debugModelThinking = "phase=model-thinking"  // + elapsed=
	debugModelOutput   = "phase=model-output"    // + elapsed=
	debugModelToolCall = "phase=model-tool-call" // + elapsed=
	debugModelActivity = "phase=model-activity"  // + elapsed=
	debugWaiting       = "phase=waiting-for-pi"  // + last-phase= silence= process=alive
	debugSettled       = "phase=settled"
	debugStarted       = "status=started"
	debugCompleted     = "status=completed"
	debugFailed        = "status=failed"
)

const (
	// debugMaxLineBytes bounds one debug line in bytes. Truncation preserves
	// valid UTF-8, so a hostile value cannot inflate a line or split a rune.
	debugMaxLineBytes = 512
	// The lane budgets sum to one less than the hard run-level bound; the
	// remaining line is reserved for the single fixed exhaustion notice.
	debugRegularLineBudget   = 315
	debugHeartbeatLineBudget = 180
	debugTerminalLineBudget  = 16
	debugBudgetNoticeLines   = 1
	debugLineBudget          = debugRegularLineBudget + debugHeartbeatLineBudget + debugTerminalLineBudget + debugBudgetNoticeLines
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
// The CLI creates one sink per run; workers obtain a worker-scoped view with
// Worker. A scope binds the worker label and owns only its liveness state;
// the writer, clock, serialization lock, and lane budgets remain run-level,
// so workers 1..3 never interleave writes or multiply the hard bound.
type DebugSink struct {
	mu                sync.Mutex
	w                 io.Writer
	start             time.Time
	now               func() time.Time
	newHeartbeatTimer func(time.Duration) debugTimer
	lines             int
	regularLines      int
	heartbeatLines    int
	terminalLines     int
	notified          bool
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
	return &DebugSink{
		w: w, start: start, now: now,
		newHeartbeatTimer: newStandardDebugTimer,
	}
}

// Worker returns the worker-scoped view of the shared run-level sink for
// the given worker id. A nil sink yields a no-op scope.
func (s *DebugSink) Worker(id int) *WorkerScope {
	return &WorkerScope{sink: s, id: id, lastPhase: "starting"}
}

// Elapsed is the run elapsed time since the sink was created, on the sink
// clock. It is nil-safe and safe for concurrent use.
func (s *DebugSink) Elapsed() time.Duration {
	if s == nil || s.now == nil {
		return 0
	}
	return s.now().Sub(s.start)
}

// WorkerScope is one worker's view of the shared run-level DebugSink. It binds
// the worker id and owns that worker's heartbeat state; the writer, clock,
// serialization lock, and lane budgets remain shared by the whole run.
type WorkerScope struct {
	sink *DebugSink
	id   int

	heartbeatMu       sync.Mutex
	heartbeatRunning  bool
	heartbeatStop     func()
	heartbeatActivity chan<- struct{}
	lastActivity      time.Duration
	lastPhase         string
}

// Log writes one sanitized lifecycle line for the scoped worker. event and
// fields must be safe, caller-constructed key=value projections; Log adds
// the worker label, replaces every control character so a hostile value
// cannot forge extra lines, truncates the line to the fixed byte bound on
// a UTF-8 boundary, and enforces the regular lane budget within the shared
// run-level bound. The first exhausted lane makes the sink emit at most one
// fixed notice for the whole run, labeled with the worker scope whose
// attempted line exhausted it, and suppresses every later line of that lane.
// Lanes are independent, so heartbeat and terminal lines can still be
// emitted after the notice. A nil scope, or one over a nil sink, is a no-op.
func (w *WorkerScope) Log(event string, fields ...string) {
	if w == nil || w.sink == nil {
		return
	}
	if phase, ok := fixedDebugPhase(event); ok {
		w.heartbeatMu.Lock()
		w.lastPhase = phase
		w.heartbeatMu.Unlock()
	}
	fields = append([]string{"worker=" + strconv.Itoa(w.id), event}, fields...)
	if w.sink.log(w, debugRegularLineKind, fields) {
		w.recordDebugActivity()
	}
}

// LogTerminal writes a settlement or final worker-status line in the
// reserved terminal lane. Terminal lines cannot be displaced by regular or
// heartbeat floods.
func (w *WorkerScope) LogTerminal(event string, fields ...string) {
	if w == nil || w.sink == nil {
		return
	}
	if phase, ok := fixedDebugPhase(event); ok {
		w.heartbeatMu.Lock()
		w.lastPhase = phase
		w.heartbeatMu.Unlock()
	}
	fields = append([]string{"worker=" + strconv.Itoa(w.id), event}, fields...)
	if w.sink.log(w, debugTerminalLineKind, fields) {
		w.recordDebugActivity()
	}
}

func fixedDebugPhase(event string) (string, bool) {
	switch event {
	case debugStarting:
		return "starting", true
	case debugThinking:
		return "thinking-confirmed", true
	case debugModelThinking:
		return "model-thinking", true
	case debugModelOutput:
		return "model-output", true
	case debugModelToolCall:
		return "model-tool-call", true
	case debugModelActivity:
		return "model-activity", true
	case debugSettled:
		return "settled", true
	default:
		return "", false
	}
}

func (w *WorkerScope) recordDebugActivity() {
	if w == nil || w.sink == nil {
		return
	}
	w.heartbeatMu.Lock()
	w.lastActivity = w.Elapsed()
	activity := w.heartbeatActivity
	w.heartbeatMu.Unlock()
	if activity != nil {
		select {
		case activity <- struct{}{}:
		default:
		}
	}
}

func (s *DebugSink) logHeartbeat(scope *WorkerScope, fields ...string) bool {
	return s.log(scope, debugHeartbeatLineKind, append([]string{"worker=" + strconv.Itoa(scope.id)}, fields...))
}

// Elapsed is the run elapsed time on the shared sink clock. It is
// nil-safe.
func (w *WorkerScope) Elapsed() time.Duration {
	if w == nil {
		return 0
	}
	return w.sink.Elapsed()
}

// enabled reports whether this scope has a real destination. It lets callers
// avoid starting background diagnostics when debug output is disabled.
func (w *WorkerScope) enabled() bool {
	return w != nil && w.sink != nil && w.sink.w != nil
}

type debugLineKind uint8

const (
	debugRegularLineKind debugLineKind = iota
	debugHeartbeatLineKind
	debugTerminalLineKind
)

// log writes one sanitized line and reports whether a line was actually
// emitted. The report drives the worker activity signal, so suppressed lines
// never postpone a heartbeat.
func (s *DebugSink) log(scope *WorkerScope, kind debugLineKind, fields []string) bool {
	if s.w == nil {
		return false
	}
	line := "[pi-worker +" + s.elapsedString() + "] " + strings.Join(fields, " ")
	line = truncateUTF8(sanitizeLine(line), debugMaxLineBytes)

	s.mu.Lock()
	defer s.mu.Unlock()

	if kind == debugTerminalLineKind {
		if s.terminalLines >= debugTerminalLineBudget || s.lines >= debugLineBudget {
			return false
		}
		s.terminalLines++
		s.lines++
		s.writeLocked(line)
		return true
	}

	laneLimit := debugRegularLineBudget
	laneLines := &s.regularLines
	if kind == debugHeartbeatLineKind {
		laneLimit = debugHeartbeatLineBudget
		laneLines = &s.heartbeatLines
	}
	if *laneLines >= laneLimit || s.lines >= debugLineBudget {
		if !s.notified && s.lines < debugLineBudget {
			s.notified = true
			s.lines++
			s.writeLocked("[pi-worker +" + s.elapsedString() + "] worker=" +
				strconv.Itoa(scope.id) + " " + debugBudgetExhausted)
			return true
		}
		return false
	}
	(*laneLines)++
	s.lines++
	s.writeLocked(line)
	return true
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

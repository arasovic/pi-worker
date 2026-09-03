package pi

import (
	"encoding/json"
	"unicode/utf8"
)

// transcriptAccumulator is the worker's salvage record of assistant text and
// the stable terminal classification as they stream. It implements
// EventHandler, appending the delta of every text_delta message_update frame,
// and retaining an assistant message's stopReason at message_end. A run that
// ends without a final text — timed out, cancelled, or failed before
// settlement — can therefore report text already produced or a stable
// assistant error. The client calls OnEvent from its single driving goroutine,
// matching the client's single-flight contract, so the accumulator needs no
// locking.
//
// Unlike usageAccumulator it must name a subtype: usage is reported on
// whichever end frame carries numbers, so that accumulator deliberately names
// no subtype and an unobserved subtype cannot break it. Text is carried on one
// specific frame type, so this accumulator names text_delta by exact string
// instead. The consequence is stated plainly: a renamed or additional
// text-carrying subtype yields empty partial text, never wrong text — a frame
// whose subtype is not text_delta is skipped, never guessed at. thinking_delta
// also carries a delta and is deliberately not accumulated: reasoning is not
// the answer, and it is not the worker's to report.
//
// The snapshot is the text the run had produced up to the interruption, not
// the last message's identity: most recent text wins, in-flight text first.
// A real run interleaves output and tool calls, so the message in flight when
// a run ends early is usually the tool call and its text buffer is empty. On
// message_start the text the ending message carried is therefore remembered
// instead of discarded, and snapshot returns the in-flight message's text
// when it carries any, otherwise the most recent remembered text.
//
// Both retained messages share one MaxFrameBytes budget, measured in UTF-8
// bytes. When a delta would exceed the budget, the oldest retained text is
// evicted first; if the current message itself is too large, its oldest prefix
// is evicted as well. Excess text therefore never remains queued for later,
// and eviction is always on a UTF-8 boundary. One lazily grown byte store
// avoids repeated string concatenation and never allocates in proportion to
// the unbounded stream.
type transcriptAccumulator struct {
	// text is the in-flight message's accumulated text_delta content. It is
	// a view into storage, immediately following mostRecent.
	text []byte
	// mostRecent is the text of the most recent message that carried text
	// before the in-flight one, remembered at message_start. It is a view
	// into the same storage as text.
	mostRecent []byte
	// storage contains mostRecent followed by text and has at most
	// MaxFrameBytes bytes. It grows geometrically up to the aggregate budget,
	// so short streams do not pay for the full ceiling up front.
	storage []byte
	// hasAssistantError records only whether the latest assistant message
	// ended with the stable error stopReason. Neither stopReason text nor the
	// errorMessage beside it is retained: both are upstream-controlled input.
	hasAssistantError bool
}

// OnEvent tracks one message boundary, text-delivery frame, or assistant
// stopReason. It never returns an error: per the EventHandler contract an
// error is a protocol violation that fails the whole client, and a salvage
// feature must never fail a run that otherwise worked. message_start promotes
// the ending message's text into mostRecent when it carried any, then starts
// the new message's buffer empty. A message_update appends its delta only when
// the assistantMessageEvent type is exactly "text_delta", so thinking and
// tool-call deltas contribute nothing. A malformed, missing, null, or
// unparseable frame contributes nothing and returns nil.
func (a *transcriptAccumulator) OnEvent(event Event) error {
	switch event.Type {
	case "message_start":
		// Only a valid assistant boundary starts a newer assistant message.
		// Other message kinds must not erase an error classification that
		// still belongs to the latest assistant turn.
		var frame struct {
			Message json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(event.Raw, &frame); err == nil {
			if assistant, _, _ := parseAssistantMessage(frame.Message); assistant {
				a.hasAssistantError = false
			}
		}
		if len(a.text) > 0 {
			// Compact the current message to the beginning of the shared
			// store and make it the remembered message. Built-in copy handles
			// the overlapping source and destination without allocation.
			textLen := len(a.text)
			copy(a.storage[:textLen], a.text)
			a.storage = a.storage[:textLen]
			a.mostRecent = a.storage
			a.text = a.storage[textLen:textLen]
		} else {
			// Keep the last non-empty message when a tool or empty message
			// starts. The empty in-flight view remains at the store's end.
			a.text = a.storage[len(a.storage):len(a.storage)]
		}
	case "message_update":
		var frame struct {
			AssistantMessageEvent *struct {
				Type  string          `json:"type"`
				Delta json.RawMessage `json:"delta"`
			} `json:"assistantMessageEvent"`
		}
		if err := json.Unmarshal(event.Raw, &frame); err != nil {
			return nil
		}
		if frame.AssistantMessageEvent == nil || frame.AssistantMessageEvent.Type != "text_delta" {
			return nil
		}
		var delta string
		if err := json.Unmarshal(frame.AssistantMessageEvent.Delta, &delta); err != nil {
			return nil
		}
		a.appendDelta(delta)
	case "message_end":
		// message_end.message is Pi's authoritative complete assistant
		// message. Keep only its stable stopReason classification; the
		// adjacent errorMessage is provider-controlled prose and is not
		// safe to project into the worker result. A known assistant message
		// with a missing or malformed stopReason explicitly clears the old
		// classification rather than inheriting it.
		var frame struct {
			Message json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(event.Raw, &frame); err != nil {
			return nil
		}
		if assistant, stopReason, validStopReason := parseAssistantMessage(frame.Message); assistant {
			a.hasAssistantError = validStopReason && stopReason == "error"
		}
	}
	return nil
}

// appendDelta appends one already-decoded text_delta while keeping the
// aggregate store bounded. It discards old retained bytes before current text,
// and only then discards a prefix of the incoming delta if that delta alone is
// larger than the budget. JSON decoding guarantees a valid UTF-8 string; the
// boundary helper also makes this invariant explicit.
func (a *transcriptAccumulator) appendDelta(delta string) {
	if delta == "" {
		return
	}
	if !utf8.ValidString(delta) {
		// encoding/json replaces invalid UTF-8 while decoding strings. Keep
		// this guard for callers that construct an Event with a custom
		// decoder: a salvage buffer must never expose invalid UTF-8.
		delta = string([]rune(delta))
	}
	if len(delta) >= MaxFrameBytes {
		// The incoming delta alone fills the budget. Drop all prior text
		// and retain its largest valid UTF-8 suffix.
		a.storage = a.storage[:0]
		a.mostRecent = a.storage
		a.text = a.storage
		delta = utf8Suffix(delta, MaxFrameBytes)
	} else if need := len(a.storage) + len(delta) - MaxFrameBytes; need > 0 {
		// Drop enough old text that the whole delta can be copied without
		// growing storage. dropPrefix may advance a little farther to
		// reach a UTF-8 boundary, leaving a few budget bytes unused.
		a.dropPrefix(need)
	}
	a.ensureCapacity(len(a.storage) + len(delta))
	a.storage = append(a.storage, delta...)
	a.setViews()
}

// ensureCapacity grows storage explicitly so append never selects a capacity
// beyond MaxFrameBytes. The initial allocation is exactly the first required
// length; subsequent allocations double geometrically, subject to the bound.
func (a *transcriptAccumulator) ensureCapacity(required int) {
	if required <= cap(a.storage) {
		return
	}
	newCap := cap(a.storage) * 2
	if newCap < required {
		newCap = required
	}
	if newCap > MaxFrameBytes {
		newCap = MaxFrameBytes
	}
	grown := make([]byte, len(a.storage), newCap)
	copy(grown, a.storage)
	a.storage = grown
	a.setViews()
}

// dropPrefix removes at least want bytes from the aggregate, advancing to a
// UTF-8 rune boundary. The aggregate is mostRecent followed by text, so this
// naturally evicts the remembered message before the in-flight message.
func (a *transcriptAccumulator) dropPrefix(want int) {
	if want <= 0 || len(a.storage) == 0 {
		return
	}
	if want > len(a.storage) {
		want = len(a.storage)
	}
	cut := want
	for cut < len(a.storage) && !utf8.RuneStart(a.storage[cut]) {
		cut++
	}
	recentLen := len(a.mostRecent)
	if cut < recentLen {
		recentLen -= cut
	} else {
		recentLen = 0
	}
	copy(a.storage, a.storage[cut:])
	a.storage = a.storage[:len(a.storage)-cut]
	a.mostRecent = a.storage[:recentLen]
	a.text = a.storage[recentLen:]
}

// setViews restores the two logical message slices after storage moves.
func (a *transcriptAccumulator) setViews() {
	mostRecentLen := len(a.mostRecent)
	if mostRecentLen > len(a.storage) {
		mostRecentLen = len(a.storage)
	}
	a.mostRecent = a.storage[:mostRecentLen]
	a.text = a.storage[mostRecentLen:]
}

// utf8Suffix returns the largest valid UTF-8 suffix no longer than max bytes.
// It never splits a rune, so a budget that falls inside a rune can leave a
// few bytes unused rather than returning invalid text.
func utf8Suffix(value string, max int) string {
	if len(value) <= max {
		return value
	}
	start := len(value) - max
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	return value[start:]
}

// parseAssistantMessage accepts only an object whose role is the exact string
// "assistant". It separately reports whether stopReason was a string, so a
// missing or mistyped stopReason cannot preserve an older assistant error.
func parseAssistantMessage(raw json.RawMessage) (assistant bool, stopReason string, validStopReason bool) {
	var message struct {
		Role       json.RawMessage `json:"role"`
		StopReason json.RawMessage `json:"stopReason"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &message) != nil {
		return false, "", false
	}
	var role string
	if json.Unmarshal(message.Role, &role) != nil || role != "assistant" {
		return false, "", false
	}
	if len(message.StopReason) == 0 || string(message.StopReason) == "null" || json.Unmarshal(message.StopReason, &stopReason) != nil {
		return true, "", false
	}
	return true, stopReason, true
}

// assistantError reports the stable assistant stopReason that Pi uses when
// the model/provider turn fails. It intentionally does not expose or retain
// the accompanying errorMessage, which is raw upstream prose.
func (a *transcriptAccumulator) assistantError() bool {
	return a.hasAssistantError
}

// snapshot returns the in-flight message's text when it carries any,
// and otherwise the most recent remembered text, or "" when no
// text_delta frame at all was observed. The empty string is the honest
// absence: no text was seen, so the caller reports none.
func (a *transcriptAccumulator) snapshot() string {
	if len(a.text) > 0 {
		return string(a.text)
	}
	return string(a.mostRecent)
}

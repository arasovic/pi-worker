package pi

import (
	"encoding/json"
)

// transcriptAccumulator is the worker's salvage record of assistant text and
// the stable terminal classification as they stream. It implements
// EventHandler, appending the delta of every text_delta message_update frame,
// and retaining an assistant message's stopReason at message_end. A run that
// ends without a final text — timed out, cancelled, or failed before settlement
// — can therefore report text already produced or a stable assistant error.
// The client calls OnEvent from its single driving goroutine, matching the
// client's single-flight contract, so the accumulator needs no locking.
//
// Unlike usageAccumulator it must name a subtype: usage is reported on
// whichever end frame carries numbers, so that accumulator deliberately
// names no subtype and an unobserved subtype cannot break it. Text is
// carried on one specific frame type, so this accumulator names
// text_delta by exact string instead. The consequence is stated plainly:
// a renamed or additional text-carrying subtype yields empty partial
// text, never wrong text — a frame whose subtype is not text_delta is
// skipped, never guessed at. thinking_delta also carries a delta and is
// deliberately not accumulated: reasoning is not the answer, and it is
// not the worker's to report.
//
// The snapshot is the text the run had produced up to the interruption,
// not the last message's identity: most recent text wins, in-flight
// text first. A real run interleaves output and tool calls, so the
// message in flight when a run ends early is usually the tool call and
// its text buffer is empty — the old rule, which kept only the last
// message and described the same message get_last_assistant_text would
// describe, was the right rule for a settled run but reported nothing
// exactly on the path this accumulator exists for. On message_start the
// text the ending message carried is therefore remembered instead of
// discarded, and snapshot returns the in-flight message's text when it
// carries any, otherwise the most recent remembered text. message_end
// does not clear anything: a message that ended cleanly before an
// interruption is still the latest text and must survive. The memory
// bound, stated honestly, is at most two messages' text — the remembered
// one and the in-flight one — instead of one. There is no size cap
// beyond that: two messages' text is bounded by the model's own output
// limits, and the frame reader already bounds each frame
// (MaxFrameBytes).
type transcriptAccumulator struct {
	// text is the in-flight message's accumulated text_delta content.
	text string
	// mostRecent is the text of the most recent message that carried
	// text before the in-flight one, remembered at message_start.
	mostRecent string
	// hasAssistantError records only whether the latest assistant message
	// ended with the stable error stopReason. Neither stopReason text nor the
	// errorMessage beside it is retained: both are upstream-controlled input.
	hasAssistantError bool
}

// OnEvent tracks one message boundary, text-delivery frame, or assistant
// stopReason. It never returns an error: per the EventHandler contract an
// error is a protocol violation that fails the whole client, and a salvage
// feature must never fail a run that otherwise worked. message_start clears
// the prior terminal classification, promotes the ending message's text
// into mostRecent when it carried any, then starts the new message's buffer
// empty; a message_update appends its delta only when the
// assistantMessageEvent type is exactly "text_delta", so thinking and
// tool-call deltas contribute nothing. A malformed, missing, null, or
// unparseable frame contributes nothing and returns nil.
func (a *transcriptAccumulator) OnEvent(event Event) error {
	switch event.Type {
	case "message_start":
		// A new message supersedes the previous terminal classification.
		// Resetting at the boundary prevents an earlier failed retry from
		// being attributed to a later stream cut.
		a.hasAssistantError = false
		if a.text != "" {
			a.mostRecent = a.text
		}
		a.text = ""
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
		a.text += delta
	case "message_end":
		// message_end.message is Pi's authoritative complete assistant
		// message. Keep only its stable stopReason classification; the
		// adjacent errorMessage is provider-controlled prose and is not
		// safe to project into the worker result.
		var frame struct {
			Message *struct {
				Role       string `json:"role"`
				StopReason string `json:"stopReason"`
			} `json:"message"`
		}
		if err := json.Unmarshal(event.Raw, &frame); err != nil {
			return nil
		}
		if frame.Message != nil && frame.Message.Role == "assistant" {
			a.hasAssistantError = frame.Message.StopReason == "error"
		}
	}
	return nil
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
	if a.text != "" {
		return a.text
	}
	return a.mostRecent
}

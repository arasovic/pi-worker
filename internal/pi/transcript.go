package pi

import (
	"encoding/json"
)

// transcriptAccumulator is the worker's salvage record of the assistant
// text as it streams. It implements EventHandler, appending the delta of
// every text_delta message_update frame, so a run that ends without a
// final text — timed out, cancelled, or failed before settlement — still
// reports the text the model had already produced. The client calls
// OnEvent from its single driving goroutine, matching the client's
// single-flight contract, so the accumulator needs no locking.
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
// The buffer holds at most one message's text: message_start resets it,
// so the snapshot always describes the last assistant message, the same
// message get_last_assistant_text would describe, and memory stays
// bounded to one message. message_end does not clear it — a message that
// ended cleanly before an interruption is still the latest text and must
// survive. There is no size cap beyond that: one message's text is
// bounded by the model's own output limit, and the frame reader already
// bounds each frame (MaxFrameBytes).
type transcriptAccumulator struct {
	text string
}

// OnEvent tracks one message boundary or text-delivery frame. It never
// returns an error: per the EventHandler contract an error is a protocol
// violation that fails the whole client, and a salvage feature must never
// fail a run that otherwise worked. message_start discards the previous
// message's text; a message_update appends its delta only when the
// assistantMessageEvent type is exactly "text_delta", so thinking and
// tool-call deltas contribute nothing. A malformed, missing, null, or
// unparseable frame contributes nothing and returns nil.
func (a *transcriptAccumulator) OnEvent(event Event) error {
	switch event.Type {
	case "message_start":
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
	}
	return nil
}

// snapshot returns the accumulated text of the last assistant message, or
// "" when no text_delta frame was observed. The empty string is the
// honest absence: no text was seen, so the caller reports none.
func (a *transcriptAccumulator) snapshot() string {
	return a.text
}

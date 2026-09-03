package pi

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// messageUpdateRaw builds one message_update event frame carrying the
// given verbatim frame body. Tests pass their own expected strings as
// literals and never read them back out of this helper.
func messageUpdateRaw(raw string) Event {
	return Event{Type: "message_update", Raw: json.RawMessage(raw)}
}

func textDeltaEvent(delta string) Event {
	frame := struct {
		Type                  string `json:"type"`
		AssistantMessageEvent struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		} `json:"assistantMessageEvent"`
	}{
		Type: "message_update",
		AssistantMessageEvent: struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		}{Type: "text_delta", Delta: delta},
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		panic(err)
	}
	return messageUpdateRaw(string(raw))
}

func TestTranscriptAccumulatorGrowsSmallDeltasLazily(t *testing.T) {
	a := &transcriptAccumulator{}
	if err := a.OnEvent(event("message_start")); err != nil {
		t.Fatalf("message_start = %v", err)
	}
	if err := a.OnEvent(textDeltaEvent("x")); err != nil {
		t.Fatalf("text delta = %v", err)
	}
	if len(a.storage) > MaxFrameBytes || cap(a.storage) > MaxFrameBytes {
		t.Fatalf("storage len/cap = %d/%d, exceeds %d-byte ceiling", len(a.storage), cap(a.storage), MaxFrameBytes)
	}
	if cap(a.storage) >= MaxFrameBytes/1024 {
		t.Fatalf("initial storage capacity = %d, want far below %d", cap(a.storage), MaxFrameBytes)
	}
}

func TestTranscriptAccumulatorPreservesViewsAcrossGrowth(t *testing.T) {
	a := &transcriptAccumulator{}
	for _, ev := range []Event{
		event("message_start"),
		textDeltaEvent("old"),
	} {
		if err := a.OnEvent(ev); err != nil {
			t.Fatalf("OnEvent(%s) = %v", ev.Type, err)
		}
	}
	initialCap := cap(a.storage)
	for _, ev := range []Event{
		event("message_start"),
		textDeltaEvent("new"),
		textDeltaEvent("!"),
	} {
		if err := a.OnEvent(ev); err != nil {
			t.Fatalf("OnEvent(%s) = %v", ev.Type, err)
		}
	}
	if got := string(a.mostRecent); got != "old" {
		t.Fatalf("mostRecent = %q, want %q after growth", got, "old")
	}
	if got := string(a.text); got != "new!" {
		t.Fatalf("text = %q, want %q after growth", got, "new!")
	}
	if len(a.storage) > MaxFrameBytes || cap(a.storage) > MaxFrameBytes {
		t.Fatalf("storage len/cap = %d/%d, exceeds %d-byte ceiling", len(a.storage), cap(a.storage), MaxFrameBytes)
	}
	if cap(a.storage) <= initialCap {
		t.Fatalf("storage capacity = %d, want growth beyond initial capacity %d", cap(a.storage), initialCap)
	}
	if got := a.snapshot(); got != "new!" {
		t.Fatalf("snapshot = %q, want current text after growth", got)
	}
}

func TestTranscriptAccumulatorMaintainsUTF8Budget(t *testing.T) {
	// Every delta is a legal, much-smaller-than-MaxFrameBytes frame. The
	// first message fills half the aggregate budget, then a newer message
	// exceeds the remaining half. The newer message is the useful salvage,
	// so its bytes evict the older message's prefix rather than stopping.
	chunk := "x" + strings.Repeat("é", 8191) + "y" // exactly 16 KiB
	if len(chunk) != 16<<10 || !utf8.ValidString(chunk) {
		t.Fatalf("test chunk = %d bytes, valid UTF-8 = %v", len(chunk), utf8.ValidString(chunk))
	}
	a := &transcriptAccumulator{}
	if err := a.OnEvent(event("message_start")); err != nil {
		t.Fatalf("first message_start = %v", err)
	}
	for i := 0; i < MaxFrameBytes/(2*len(chunk)); i++ {
		if err := a.OnEvent(textDeltaEvent(chunk)); err != nil {
			t.Fatalf("first delta %d = %v", i, err)
		}
	}
	if err := a.OnEvent(event("message_start")); err != nil {
		t.Fatalf("second message_start = %v", err)
	}
	for i := 0; i < MaxFrameBytes/len(chunk)/2+1; i++ {
		if err := a.OnEvent(textDeltaEvent(chunk)); err != nil {
			t.Fatalf("second delta %d = %v", i, err)
		}
	}

	if len(a.storage) > MaxFrameBytes {
		t.Fatalf("storage length = %d, exceeds shared ceiling %d", len(a.storage), MaxFrameBytes)
	}
	if cap(a.storage) > MaxFrameBytes {
		t.Fatalf("storage capacity = %d, exceeds shared ceiling %d", cap(a.storage), MaxFrameBytes)
	}
	if len(a.text) == 0 || len(a.mostRecent) == 0 {
		t.Fatalf("retained buffers = text %d, mostRecent %d; want both populated", len(a.text), len(a.mostRecent))
	}
	if !utf8.Valid(a.text) || !utf8.Valid(a.mostRecent) || !utf8.ValidString(a.snapshot()) {
		t.Fatal("retained transcript is not valid UTF-8")
	}
	wantSecond := strings.Repeat(chunk, MaxFrameBytes/len(chunk)/2+1)
	if got := a.snapshot(); got != wantSecond {
		t.Fatalf("snapshot retained %d bytes, want the complete newer message of %d bytes", len(got), len(wantSecond))
	}
}

func TestTranscriptAccumulatorEvictsOldestTextWhenCurrentExceedsBudget(t *testing.T) {
	// Once the current message itself is larger than the budget, the oldest
	// prefix of that current message is evicted too. This pins a recent-tail
	// policy instead of silently returning an unbounded transcript.
	chunk := strings.Repeat("界", 4096) // 12 KiB and valid UTF-8
	a := &transcriptAccumulator{}
	if err := a.OnEvent(event("message_start")); err != nil {
		t.Fatalf("message_start = %v", err)
	}
	for i := 0; i < MaxFrameBytes/len(chunk)+1; i++ {
		if err := a.OnEvent(textDeltaEvent(chunk)); err != nil {
			t.Fatalf("delta %d = %v", i, err)
		}
	}
	if len(a.storage) > MaxFrameBytes {
		t.Fatalf("storage length = %d, exceeds shared ceiling %d", len(a.storage), MaxFrameBytes)
	}
	if cap(a.storage) > MaxFrameBytes {
		t.Fatalf("storage capacity = %d, exceeds shared ceiling %d", cap(a.storage), MaxFrameBytes)
	}
	if got := len(a.snapshot()); got != MaxFrameBytes-2 {
		t.Fatalf("snapshot = %d bytes, want the largest valid UTF-8 suffix %d", got, MaxFrameBytes-2)
	}
	if !utf8.ValidString(a.snapshot()) {
		t.Fatal("evicted snapshot is not valid UTF-8")
	}
}

func TestTranscriptAccumulatorKeepsEarlierTextAcrossToolCallMessage(t *testing.T) {
	// text, then a new message carrying a tool-call delta: the snapshot
	// still returns the earlier text. This is the test that pins the
	// whole fix: a real run interleaves output and tool calls, so the
	// message in flight when a run ends early is usually the tool call,
	// and the assistant text the model already wrote must survive that
	// message's message_start.
	a := &transcriptAccumulator{}
	stream := []Event{
		event("message_start"),
		messageUpdateRaw(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"first part"}}`),
		event("message_start"),
		messageUpdateRaw(`{"type":"message_update","assistantMessageEvent":{"type":"toolcall_delta","delta":"tool payload"}}`),
	}
	for i, ev := range stream {
		if err := a.OnEvent(ev); err != nil {
			t.Fatalf("OnEvent(frame %d) = %v, want nil", i, err)
		}
	}
	if got := a.snapshot(); got != "first part" {
		t.Fatalf("snapshot = %q, want %q: the earlier text must survive a tool-call message", got, "first part")
	}
}

func TestTranscriptAccumulatorNewerTextWinsOverRememberedText(t *testing.T) {
	// text, tool-call message, then new text: the newer text wins over
	// the remembered earlier text, because the in-flight message's text
	// is what the model is producing right now.
	a := &transcriptAccumulator{}
	stream := []Event{
		event("message_start"),
		messageUpdateRaw(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"first part"}}`),
		event("message_start"),
		messageUpdateRaw(`{"type":"message_update","assistantMessageEvent":{"type":"toolcall_delta","delta":"tool payload"}}`),
		event("message_start"),
		messageUpdateRaw(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"second part."}}`),
	}
	for i, ev := range stream {
		if err := a.OnEvent(ev); err != nil {
			t.Fatalf("OnEvent(frame %d) = %v, want nil", i, err)
		}
	}
	if got := a.snapshot(); got != "second part." {
		t.Fatalf("snapshot = %q, want %q: the in-flight text must win over the remembered text", got, "second part.")
	}
}

func TestTranscriptAccumulatorEmptyInFlightFallsBackToRememberedText(t *testing.T) {
	// An in-flight message that has produced no text yet after a message
	// that had text: the earlier text is returned, not the empty buffer.
	a := &transcriptAccumulator{}
	stream := []Event{
		event("message_start"),
		messageUpdateRaw(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"first part"}}`),
		event("message_start"),
		// The new message is in flight with no text_delta yet.
	}
	for i, ev := range stream {
		if err := a.OnEvent(ev); err != nil {
			t.Fatalf("OnEvent(frame %d) = %v, want nil", i, err)
		}
	}
	if got := a.snapshot(); got != "first part" {
		t.Fatalf("snapshot = %q, want %q: an empty in-flight message must fall back to the remembered text", got, "first part")
	}
}

func TestTranscriptAccumulatorDeltasConcatenateInOrder(t *testing.T) {
	// text_delta frames arrive in stream order and each carries a slice
	// of the message text: the snapshot is their concatenation, in the
	// order the frames arrived.
	a := &transcriptAccumulator{}
	stream := []Event{
		event("message_start"),
		messageUpdateRaw(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"Hello"}}`),
		messageUpdateRaw(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":", "}}`),
		messageUpdateRaw(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"world"}}`),
	}
	for i, ev := range stream {
		if err := a.OnEvent(ev); err != nil {
			t.Fatalf("OnEvent(frame %d) = %v, want nil", i, err)
		}
	}
	if got := a.snapshot(); got != "Hello, world" {
		t.Fatalf("snapshot = %q, want the concatenated deltas %q", got, "Hello, world")
	}
}

func TestTranscriptAccumulatorMessageStartMakesNewerTextWin(t *testing.T) {
	// The snapshot is the partial counterpart of explanation, which is the
	// current message's text when present: a newer in-flight message wins
	// over remembered text. The shared aggregate still retains the older
	// message for the recent-tail policy, subject to the byte budget.
	a := &transcriptAccumulator{}
	stream := []Event{
		event("message_start"),
		messageUpdateRaw(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"first message"}}`),
		event("message_start"),
		messageUpdateRaw(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"second"}}`),
	}
	for i, ev := range stream {
		if err := a.OnEvent(ev); err != nil {
			t.Fatalf("OnEvent(frame %d) = %v, want nil", i, err)
		}
	}
	if got := a.snapshot(); got != "second" {
		t.Fatalf("snapshot = %q, want %q: the earlier message's text must not survive message_start", got, "second")
	}
}

func TestTranscriptAccumulatorNonTextSubtypesContributeNothing(t *testing.T) {
	// thinking_delta and toolcall_delta also carry a delta. They must not
	// be accumulated: reasoning is not the answer, and tool-call payloads
	// are not assistant text. The text frames around them still land.
	a := &transcriptAccumulator{}
	stream := []Event{
		event("message_start"),
		messageUpdateRaw(`{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"hidden reasoning"}}`),
		messageUpdateRaw(`{"type":"message_update","assistantMessageEvent":{"type":"toolcall_delta","delta":"tool payload"}}`),
		messageUpdateRaw(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"answer"}}`),
		messageUpdateRaw(`{"type":"message_update","assistantMessageEvent":{"type":"toolcall_delta","delta":"more payload"}}`),
	}
	for i, ev := range stream {
		if err := a.OnEvent(ev); err != nil {
			t.Fatalf("OnEvent(frame %d) = %v, want nil", i, err)
		}
	}
	if got := a.snapshot(); got != "answer" {
		t.Fatalf("snapshot = %q, want %q: only text_delta frames contribute", got, "answer")
	}
}

func TestTranscriptAccumulatorMalformedFramesContributeNothing(t *testing.T) {
	// A malformed, missing, null, or unparseable assistantMessageEvent
	// contributes nothing and returns no error: per the EventHandler
	// contract an error is a protocol violation that fails the whole
	// client, so a salvage feature must never fail a run that otherwise
	// worked. The missing-type frame also pins the exact-match rule: a
	// frame carrying a delta without the text_delta type is not text.
	malformed := []Event{
		messageUpdateRaw(`{"type":"message_update"}`),
		messageUpdateRaw(`{"type":"message_update","assistantMessageEvent":null}`),
		messageUpdateRaw(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta"}}`),
		messageUpdateRaw(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":null}}`),
		messageUpdateRaw(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":42}}`),
		messageUpdateRaw(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":{"text":"not a string"}}}`),
		messageUpdateRaw(`{"type":"message_update","assistantMessageEvent":"not an object"}`),
		messageUpdateRaw(`{"type":"message_update","assistantMessageEvent":{"delta":"no type"}}`),
		messageUpdateRaw(`not json`),
	}
	a := &transcriptAccumulator{}
	stream := []Event{event("message_start")}
	stream = append(stream, malformed...)
	for i, ev := range stream {
		if err := a.OnEvent(ev); err != nil {
			t.Fatalf("OnEvent(malformed %d) = %v, want nil: a handler error fails the client", i, err)
		}
	}
	if got := a.snapshot(); got != "" {
		t.Fatalf("snapshot = %q, want empty: malformed frames must not contribute", got)
	}

	// A valid message after the malformed frames is still accumulated in
	// full: a skipped frame must not poison the message.
	for _, ev := range []Event{
		event("message_start"),
		messageUpdateRaw(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"valid"}}`),
	} {
		if err := a.OnEvent(ev); err != nil {
			t.Fatalf("OnEvent(valid) = %v, want nil", err)
		}
	}
	if got := a.snapshot(); got != "valid" {
		t.Fatalf("snapshot = %q, want %q: a valid frame after malformed ones must accumulate", got, "valid")
	}
}

func TestTranscriptAccumulatorMessageEndKeepsText(t *testing.T) {
	// A message that ended cleanly before the interruption is still the
	// latest text: message_end must not clear the buffer, or a run killed
	// after the message finished would report nothing.
	a := &transcriptAccumulator{}
	for _, ev := range []Event{
		event("message_start"),
		messageUpdateRaw(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"finished"}}`),
		event("message_end"),
	} {
		if err := a.OnEvent(ev); err != nil {
			t.Fatalf("OnEvent = %v, want nil", err)
		}
	}
	if got := a.snapshot(); got != "finished" {
		t.Fatalf("snapshot = %q, want %q: message_end must not clear the text", got, "finished")
	}
}

func TestTranscriptAssistantErrorStateFollowsAssistantBoundaries(t *testing.T) {
	// A failed low-level attempt can be followed by agent_end(willRetry:true)
	// and a retry. User and tool-result messages do not start a newer
	// assistant turn, while the retry's assistant message supersedes the old
	// error even when its stopReason is missing or malformed.
	tests := []struct {
		name       string
		messageEnd string
	}{
		{name: "missing stop reason", messageEnd: `{"type":"message_end","message":{"role":"assistant"}}`},
		{name: "malformed stop reason", messageEnd: `{"type":"message_end","message":{"role":"assistant","stopReason":42}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a := &transcriptAccumulator{}
			stream := []Event{
				{Type: "message_start", Raw: json.RawMessage(`{"type":"message_start","message":{"role":"assistant"}}`)},
				{Type: "message_end", Raw: json.RawMessage(`{"type":"message_end","message":{"role":"assistant","stopReason":"error"}}`)},
				{Type: "agent_end", Raw: json.RawMessage(`{"type":"agent_end","willRetry":true}`)},
				{Type: "message_start", Raw: json.RawMessage(`{"type":"message_start","message":{"role":"user"}}`)},
			}
			for _, ev := range stream {
				if err := a.OnEvent(ev); err != nil {
					t.Fatalf("OnEvent(%s) = %v, want nil", ev.Type, err)
				}
			}
			if !a.assistantError() {
				t.Fatal("user message_start cleared the prior assistant error")
			}
			if err := a.OnEvent(Event{Type: "message_start", Raw: json.RawMessage(`{"type":"message_start","message":{"role":"toolResult"}}`)}); err != nil {
				t.Fatalf("tool-result message_start = %v, want nil", err)
			}
			if !a.assistantError() {
				t.Fatal("tool-result message_start cleared the prior assistant error")
			}
			if err := a.OnEvent(Event{Type: "message_start", Raw: json.RawMessage(`{"type":"message_start","message":{"role":"assistant"}}`)}); err != nil {
				t.Fatalf("retry assistant message_start = %v, want nil", err)
			}
			if a.assistantError() {
				t.Fatal("new assistant message_start did not clear the prior error")
			}
			if err := a.OnEvent(Event{Type: "message_end", Raw: json.RawMessage(test.messageEnd)}); err != nil {
				t.Fatalf("retry message_end = %v, want nil", err)
			}
			if a.assistantError() {
				t.Fatal("missing or malformed retry stopReason inherited the prior error")
			}
		})
	}
}

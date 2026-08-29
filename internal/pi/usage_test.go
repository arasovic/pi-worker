package pi

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/arasovic/pi-worker/internal/testutil/fakepi/script"
)

// zeroUsage is the all-zero usage object openai-codex reported on every
// frame of a tool-using run: that provider sends no numbers, ever.
const zeroUsage = `{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}`

// event builds one non-message_update event frame of the given type.
func event(typ string) Event {
	return Event{Type: typ, Raw: json.RawMessage(`{"type":"` + typ + `"}`)}
}

// messageUpdate builds one message_update event frame carrying usage and
// the given assistantMessageEvent subtype. Tests pass their own expected
// numbers as literals and never read them back out of this helper. The
// subtypes observed so far on the wire against Pi 0.84.4 are
// thinking_start, thinking_delta, thinking_end, text_start, text_delta,
// text_end, toolcall_start, toolcall_delta, and toolcall_end — that list
// is what has been observed, not a closed set, and none of its members is
// the "done" or "error" the old tests assumed.
func messageUpdate(usage, subtype string) Event {
	return Event{
		Type: "message_update",
		Raw:  json.RawMessage(`{"type":"message_update","usage":` + usage + `,"assistantMessageEvent":{"type":"` + subtype + `"}}`),
	}
}

func TestUsageAccumulatorMeasuredTraceEndToEnd(t *testing.T) {
	// The exact qwen-token-plan stream observed on the wire against Pi
	// 0.84.4, frame for frame. The first message carries no message_update
	// frames at all; the second message's text_start and text_delta frames
	// report all-zero usage, and only its text_end frame carries numbers.
	// Neither message_end carries a usage field, and no "done" or "error"
	// frame exists anywhere in the stream.
	a := &usageAccumulator{}
	stream := []Event{
		event("thinking_level_changed"),
		event("agent_start"),
		event("turn_start"),
		event("message_start"),
		event("message_end"),
		event("message_start"),
		messageUpdate(zeroUsage, "text_start"),
		messageUpdate(zeroUsage, "text_delta"),
		messageUpdate(`{"input":116,"output":1,"cacheRead":2048,"cacheWrite":0,"reasoning":0,"totalTokens":2165,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}`, "text_end"),
		event("message_end"),
		event("turn_end"),
		event("agent_end"),
		event("agent_settled"),
	}
	for i, ev := range stream {
		if err := a.OnEvent(ev); err != nil {
			t.Fatalf("OnEvent(frame %d) = %v, want nil", i, err)
		}
	}
	got := a.snapshot()
	want := &Usage{
		Input: 116, Output: 1, CacheRead: 2048, CacheWrite: 0, TotalTokens: 2165,
		Reasoning: intPtr(0),
		Cost:      UsageCost{},
	}
	if got == nil {
		t.Fatalf("snapshot = nil, want the measured trace's usage present")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %#v, want the text_end frame's numbers %#v", got, want)
	}
}

func TestUsageAccumulatorSilentProviderUsageAbsent(t *testing.T) {
	// openai-codex reported an all-zero usage object on every frame of a
	// tool-using run: that provider sends no numbers, ever. A message
	// bounded by message_start and message_end that reports only zeros was
	// never measured, so the snapshot must be nil — asserted as nil, not
	// as a struct of zero fields.
	a := &usageAccumulator{}
	stream := []Event{event("message_start")}
	// All eight observed subtypes: none of them carries a measurement here.
	for _, subtype := range []string{
		"thinking_start", "thinking_end",
		"text_start", "text_delta", "text_end",
		"toolcall_start", "toolcall_delta", "toolcall_end",
	} {
		stream = append(stream, messageUpdate(zeroUsage, subtype))
	}
	stream = append(stream, event("message_end"))
	for i, ev := range stream {
		if err := a.OnEvent(ev); err != nil {
			t.Fatalf("OnEvent(frame %d) = %v, want nil", i, err)
		}
	}
	if got := a.snapshot(); got != nil {
		t.Fatalf("snapshot = %#v, want nil: an all-zero report is no measurement", got)
	}
}

func TestUsageAccumulatorTwoMessagesCountedOnceEach(t *testing.T) {
	// Two complete messages, each reporting its final figure on its last
	// frame. The message_end boundary commits each message exactly once,
	// and snapshot's final commit must not add either message again: the
	// total is their plain sum. The cost figures are binary-exact so the
	// float64 additions are exact and the literals compare cleanly.
	a := &usageAccumulator{}
	first := `{"input":100,"output":20,"cacheRead":5,"cacheWrite":2,"totalTokens":127,"cost":{"input":0.5,"output":0.0625,"cacheRead":0.125,"cacheWrite":0.25,"total":1.0}}`
	second := `{"input":30,"output":10,"cacheRead":3,"cacheWrite":1,"totalTokens":44,"cost":{"input":0.25,"output":0.03125,"cacheRead":0.0625,"cacheWrite":0.5,"total":0.875}}`
	stream := []Event{
		event("message_start"),
		messageUpdate(first, "text_delta"),
		messageUpdate(first, "text_end"),
		event("message_end"),
		event("message_start"),
		messageUpdate(second, "text_start"),
		messageUpdate(second, "text_end"),
		event("message_end"),
	}
	for i, ev := range stream {
		if err := a.OnEvent(ev); err != nil {
			t.Fatalf("OnEvent(frame %d) = %v, want nil", i, err)
		}
	}
	got := a.snapshot()
	want := &Usage{
		Input: 130, Output: 30, CacheRead: 8, CacheWrite: 3, TotalTokens: 171,
		Cost: UsageCost{Input: 0.75, Output: 0.09375, CacheRead: 0.1875, CacheWrite: 0.75, Total: 1.875},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %#v, want the sum of both messages %#v, each counted once", got, want)
	}
}

func TestUsageAccumulatorZeroFramesBeforeFinalFigureCountOnce(t *testing.T) {
	// Inside one message, text_start and text_delta report zeros, a
	// mid-message frame reports a partial figure (the cumulative fills the
	// old code assumed), and text_end carries the message's final numbers.
	// The last reported usage is the measurement: the earlier zeros must
	// not overwrite it, and the earlier partial must be replaced, not
	// added to it.
	a := &usageAccumulator{}
	final := `{"input":45,"output":35,"cacheRead":10,"cacheWrite":5,"totalTokens":95,"cost":{"input":0.00045,"output":0.00035,"cacheRead":0.00010,"cacheWrite":0.00005,"total":0.00095}}`
	partial := `{"input":20,"output":15,"cacheRead":4,"cacheWrite":2,"totalTokens":41,"cost":{"input":0.00020,"output":0.00015,"cacheRead":0.00004,"cacheWrite":0.00002,"total":0.00041}}`
	stream := []Event{
		event("message_start"),
		messageUpdate(zeroUsage, "text_start"),
		messageUpdate(zeroUsage, "text_delta"),
		messageUpdate(partial, "text_delta"),
		messageUpdate(final, "text_end"),
		event("message_end"),
	}
	for i, ev := range stream {
		if err := a.OnEvent(ev); err != nil {
			t.Fatalf("OnEvent(frame %d) = %v, want nil", i, err)
		}
	}
	got := a.snapshot()
	want := &Usage{
		Input: 45, Output: 35, CacheRead: 10, CacheWrite: 5, TotalTokens: 95,
		Cost: UsageCost{Input: 0.00045, Output: 0.00035, CacheRead: 0.00010, CacheWrite: 0.00005, Total: 0.00095},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %#v, want the final frame's numbers %#v (replaced, not summed)", got, want)
	}
}

func TestUsageAccumulatorMeasuredToolRunSumsMessagesOnce(t *testing.T) {
	// The qwen-token-plan tool-using run measured against Pi 0.84.4, frame
	// for frame: two empty messages (message_start straight to
	// message_end) commit nothing, and the two messages that carry numbers
	// report them on the end frame of each content block — thinking_end
	// and toolcall_end both carry the tool message's cumulative 2302, and
	// text_end carries the text message's 2329. The latest reported figure
	// is each message's: summing the frames instead would produce
	// 2302+2302+2329 = 6933, while the measured document reported 4631.
	// The delta repeat counts are condensed (three thinking_delta and two
	// toolcall_delta frames instead of eleven and four); the shape is the
	// measured one — all-zero delta frames, then two end frames carrying
	// the same figure. reasoning was reported (19, plus the text message's
	// reported zero), so it is present; cacheWrite1h was never reported by
	// any frame, so it stays absent.
	a := &usageAccumulator{}
	toolMessage := `{"input":2239,"output":63,"cacheRead":0,"cacheWrite":0,"reasoning":19,"totalTokens":2302,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}`
	textMessage := `{"input":268,"output":13,"cacheRead":2048,"cacheWrite":0,"reasoning":0,"totalTokens":2329,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}`
	stream := []Event{
		event("agent_start"),
		event("turn_start"),
		event("message_start"),
		event("message_end"),
		event("message_start"),
		messageUpdate(zeroUsage, "thinking_start"),
		messageUpdate(zeroUsage, "thinking_delta"),
		messageUpdate(zeroUsage, "thinking_delta"),
		messageUpdate(zeroUsage, "thinking_delta"),
		messageUpdate(zeroUsage, "toolcall_start"),
		messageUpdate(zeroUsage, "toolcall_delta"),
		messageUpdate(zeroUsage, "toolcall_delta"),
		messageUpdate(toolMessage, "thinking_end"),
		messageUpdate(toolMessage, "toolcall_end"),
		event("message_end"),
		event("tool_execution_start"),
		event("tool_execution_end"),
		event("message_start"),
		event("message_end"),
		event("turn_end"),
		event("turn_start"),
		event("message_start"),
		messageUpdate(zeroUsage, "text_start"),
		messageUpdate(zeroUsage, "text_delta"),
		messageUpdate(zeroUsage, "text_delta"),
		messageUpdate(textMessage, "text_end"),
		event("message_end"),
	}
	for i, ev := range stream {
		if err := a.OnEvent(ev); err != nil {
			t.Fatalf("OnEvent(frame %d) = %v, want nil", i, err)
		}
	}
	got := a.snapshot()
	want := &Usage{
		Input: 2507, Output: 76, CacheRead: 2048, CacheWrite: 0, TotalTokens: 4631,
		Reasoning: intPtr(19),
		Cost:      UsageCost{},
	}
	if got == nil {
		t.Fatalf("snapshot = nil, want the measured tool run's usage present")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %#v, want %#v: each message's latest reported figure, summed once", got, want)
	}
}

func TestUsageAccumulatorUnterminatedMessageStillCounts(t *testing.T) {
	// A message whose end never arrives — a timed-out or cancelled run —
	// still contributes what it reported. The commit happens on both
	// message boundaries and in snapshot, so no commit path may lose or
	// double a message: part one pins the boundary path, part two pins the
	// snapshot path.
	first := `{"input":9,"output":2,"cacheRead":3,"cacheWrite":0,"totalTokens":14,"cost":{"input":0.000125,"output":0.00003125,"cacheRead":0.0000625,"cacheWrite":0,"total":0.00021875}}`
	second := `{"input":12,"output":4,"cacheRead":3,"cacheWrite":0,"totalTokens":19,"cost":{"input":0.00025,"output":0.0000625,"cacheRead":0.00003125,"cacheWrite":0,"total":0.00034375}}`

	// The first message never reaches message_end; the second message's
	// message_start boundary commits it, then the second message ends
	// normally. The total is the sum, counted once each.
	a := &usageAccumulator{}
	stream := []Event{
		event("message_start"),
		messageUpdate(first, "text_delta"),
		event("message_start"),
		messageUpdate(second, "text_end"),
		event("message_end"),
	}
	for i, ev := range stream {
		if err := a.OnEvent(ev); err != nil {
			t.Fatalf("OnEvent(frame %d) = %v, want nil", i, err)
		}
	}
	got := a.snapshot()
	want := &Usage{
		Input: 21, Output: 6, CacheRead: 6, CacheWrite: 0, TotalTokens: 33,
		Cost: UsageCost{Input: 0.000375, Output: 0.00009375, CacheRead: 0.00009375, CacheWrite: 0, Total: 0.0005625},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %#v, want the sum %#v: the unterminated message must count", got, want)
	}

	// A stream that stops outright with no later boundary at all:
	// snapshot itself commits the pending message.
	b := &usageAccumulator{}
	streamB := []Event{
		event("message_start"),
		messageUpdate(second, "text_start"),
		messageUpdate(second, "text_end"),
	}
	for i, ev := range streamB {
		if err := b.OnEvent(ev); err != nil {
			t.Fatalf("OnEvent(frame %d) = %v, want nil", i, err)
		}
	}
	gotB := b.snapshot()
	wantB := &Usage{
		Input: 12, Output: 4, CacheRead: 3, CacheWrite: 0, TotalTokens: 19,
		Cost: UsageCost{Input: 0.00025, Output: 0.0000625, CacheRead: 0.00003125, CacheWrite: 0, Total: 0.00034375},
	}
	if !reflect.DeepEqual(gotB, wantB) {
		t.Fatalf("snapshot = %#v, want %#v: snapshot must commit what message_end never did", gotB, wantB)
	}
}

func TestUsageAccumulatorMalformedUsageSkippedAndRunContinues(t *testing.T) {
	// usage that is missing, null, empty, undecodable, or carrying a
	// negative number is skipped silently: counted never, failing never.
	// OnEvent must never return an error: per the EventHandler contract an
	// error is a protocol violation that fails the whole client, so a
	// measurement problem would fail a run that otherwise worked.
	malformed := []string{
		`{"type":"message_update","usage":{},"assistantMessageEvent":{"type":"text_end"}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_end"}}`,
		`{"type":"message_update","usage":null,"assistantMessageEvent":{"type":"text_end"}}`,
		`{"type":"message_update","usage":42,"assistantMessageEvent":{"type":"text_end"}}`,
		`{"type":"message_update","usage":"not an object","assistantMessageEvent":{"type":"text_end"}}`,
		`{"type":"message_update","usage":{"input":"not a number","output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":1,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"assistantMessageEvent":{"type":"text_end"}}`,
		`{"type":"message_update","usage":{"input":-5,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"assistantMessageEvent":{"type":"text_end"}}`,
		`{"type":"message_update","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0,"cost":{"input":-0.5,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"assistantMessageEvent":{"type":"text_end"}}`,
	}
	a := &usageAccumulator{}
	negative := malformed[6]
	valid := `{"input":7,"output":2,"cacheRead":0,"cacheWrite":0,"totalTokens":9,"cost":{"input":0.00007,"output":0.00002,"cacheRead":0,"cacheWrite":0,"total":0.00009}}`

	// A message composed entirely of malformed frames contributes nothing.
	stream := []Event{event("message_start")}
	for _, raw := range malformed {
		stream = append(stream, Event{Type: "message_update", Raw: json.RawMessage(raw)})
	}
	stream = append(stream, event("message_end"))
	for i, ev := range stream {
		if err := a.OnEvent(ev); err != nil {
			t.Fatalf("OnEvent(malformed %d) = %v, want nil: a handler error fails the client", i, err)
		}
	}
	if got := a.snapshot(); got != nil {
		t.Fatalf("snapshot = %#v, want nil: malformed usage must not be counted", got)
	}

	// A valid message after the malformed one is still counted in full.
	for _, ev := range []Event{
		event("message_start"),
		messageUpdate(valid, "text_end"),
		event("message_end"),
	} {
		if err := a.OnEvent(ev); err != nil {
			t.Fatalf("OnEvent(valid) = %v, want nil", err)
		}
	}
	got := a.snapshot()
	want := &Usage{
		Input: 7, Output: 2, CacheRead: 0, CacheWrite: 0, TotalTokens: 9,
		Cost: UsageCost{Input: 0.00007, Output: 0.00002, CacheRead: 0, CacheWrite: 0, Total: 0.00009},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %#v, want only the valid message's numbers %#v", got, want)
	}

	// A negative frame inside an otherwise valid message must not disturb
	// the message's pending figure: it is skipped, not remembered.
	b := &usageAccumulator{}
	for _, ev := range []Event{
		event("message_start"),
		messageUpdate(valid, "text_delta"),
		Event{Type: "message_update", Raw: json.RawMessage(negative)},
		event("message_end"),
	} {
		if err := b.OnEvent(ev); err != nil {
			t.Fatalf("OnEvent = %v, want nil", err)
		}
	}
	gotB := b.snapshot()
	if !reflect.DeepEqual(gotB, want) {
		t.Fatalf("snapshot = %#v, want the valid frame's numbers %#v: a negative frame must not overwrite pending usage", gotB, want)
	}
}

func TestUsageAccumulatorOptionalFieldsAbsentUntilReportedAndSum(t *testing.T) {
	// cacheWrite1h and reasoning are omitted when no message reported
	// them, and summed when more than one message did.
	withFirst := `{"input":40,"output":9,"cacheRead":0,"cacheWrite":1,"cacheWrite1h":5,"reasoning":7,"totalTokens":50,"cost":{"input":0.0004,"output":0.00009,"cacheRead":0,"cacheWrite":0.00001,"total":0.0005}}`
	withSecond := `{"input":30,"output":7,"cacheRead":0,"cacheWrite":0,"cacheWrite1h":2,"reasoning":3,"totalTokens":37,"cost":{"input":0.0003,"output":0.00007,"cacheRead":0,"cacheWrite":0,"total":0.00037}}`
	without := `{"input":30,"output":7,"cacheRead":0,"cacheWrite":0,"totalTokens":37,"cost":{"input":0.0003,"output":0.00007,"cacheRead":0,"cacheWrite":0,"total":0.00037}}`

	a := &usageAccumulator{}
	stream := []Event{
		event("message_start"),
		messageUpdate(withFirst, "text_end"),
		event("message_end"),
		event("message_start"),
		messageUpdate(withSecond, "text_end"),
		event("message_end"),
		event("message_start"),
		messageUpdate(without, "text_end"),
		event("message_end"),
	}
	for i, ev := range stream {
		if err := a.OnEvent(ev); err != nil {
			t.Fatalf("OnEvent(frame %d) = %v, want nil", i, err)
		}
	}
	got := a.snapshot()
	if got == nil {
		t.Fatalf("snapshot = nil, want present usage")
	}
	if got.CacheWrite1h == nil || *got.CacheWrite1h != 7 {
		t.Fatalf("cacheWrite1h = %v, want 7: reported twice, summed once each", got.CacheWrite1h)
	}
	if got.Reasoning == nil || *got.Reasoning != 10 {
		t.Fatalf("reasoning = %v, want 10", got.Reasoning)
	}
	if got.Input != 100 || got.Output != 23 || got.TotalTokens != 124 {
		t.Fatalf("totals = %#v, want input 100 output 23 totalTokens 124", got)
	}

	// A message that reported only a non-zero optional figure is still a
	// measurement: the snapshot is present, not nil.
	b := &usageAccumulator{}
	for _, ev := range []Event{
		event("message_start"),
		messageUpdate(`{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"cacheWrite1h":1,"totalTokens":0,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}`, "text_end"),
		event("message_end"),
	} {
		if err := b.OnEvent(ev); err != nil {
			t.Fatalf("OnEvent = %v, want nil", err)
		}
	}
	gotB := b.snapshot()
	if gotB == nil || gotB.CacheWrite1h == nil || *gotB.CacheWrite1h != 1 {
		t.Fatalf("snapshot = %#v, want present usage with cacheWrite1h 1", gotB)
	}

	// Never-reported optionals stay absent from the document, never a zero
	// pi-worker invented.
	c := &usageAccumulator{}
	for _, ev := range []Event{
		event("message_start"),
		messageUpdate(without, "text_end"),
		event("message_end"),
	} {
		if err := c.OnEvent(ev); err != nil {
			t.Fatalf("OnEvent = %v, want nil", err)
		}
	}
	gotC := c.snapshot()
	if gotC == nil {
		t.Fatalf("snapshot = nil, want present usage")
	}
	if gotC.CacheWrite1h != nil || gotC.Reasoning != nil {
		t.Fatalf("optional fields = %v/%v, want absent", gotC.CacheWrite1h, gotC.Reasoning)
	}
	data, err := json.Marshal(gotC)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
	for _, key := range []string{"cacheWrite1h", "reasoning"} {
		if _, present := object[key]; present {
			t.Fatalf("JSON %s carries %q, want it omitted", data, key)
		}
	}
}

func TestWorkerReportsUsageOnFailedRun(t *testing.T) {
	// A run that fails after its message stream still spent money, and the
	// document is what records the spend: the result carries both the
	// failure status and the usage. The script injects the real event
	// vocabulary — message_start and message_end boundaries around
	// message_update frames, with no "done" or "error" frame anywhere.
	scriptConfig := happyPathScript("unused")
	scriptConfig.Triggers["prompt"] = []script.Step{
		{Response: &script.Response{Success: true}},
		{Event: json.RawMessage(`{"type":"message_start","message":{"role":"assistant","content":[]}}`)},
		{Event: json.RawMessage(`{"type":"message_update","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"assistantMessageEvent":{"type":"text_start","contentIndex":0}}`)},
		{Event: json.RawMessage(`{"type":"message_update","usage":{"input":900,"output":60,"cacheRead":100,"cacheWrite":4,"cacheWrite1h":2,"reasoning":120,"totalTokens":1084,"cost":{"input":0.009,"output":0.003,"cacheRead":0.001,"cacheWrite":0.0002,"total":0.0132}},"assistantMessageEvent":{"type":"text_end","contentIndex":0}}`)},
		{Event: json.RawMessage(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"go"}]}}`)},
		{Event: json.RawMessage(`{"type":"agent_settled"}`)},
	}
	scriptConfig.Triggers["get_last_assistant_text"] = []script.Step{{Response: &script.Response{Success: false, Error: "model error"}}}
	setupFakePiEnv(t, scriptConfig)

	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
	})

	if result.Status != StatusFailed {
		t.Fatalf("status = %q, error = %q, want failed", result.Status, result.Error)
	}
	if result.Usage == nil {
		t.Fatalf("usage = nil, want the failed run's spend reported")
	}
	want := &Usage{
		Input: 900, Output: 60, CacheRead: 100, CacheWrite: 4, TotalTokens: 1084,
		CacheWrite1h: intPtr(2), Reasoning: intPtr(120),
		Cost: UsageCost{Input: 0.009, Output: 0.003, CacheRead: 0.001, CacheWrite: 0.0002, Total: 0.0132},
	}
	if !reflect.DeepEqual(result.Usage, want) {
		t.Fatalf("usage = %#v, want %#v", result.Usage, want)
	}
}

func intPtr(value int) *int {
	return &value
}

func TestWorkerCompletedRunReportsUsage(t *testing.T) {
	// The completed path funnels through the same withThinking wrapper as
	// every other status, so a successful run carries the same shape —
	// again over the real event vocabulary, boundaries included.
	scriptConfig := happyPathScript("answer")
	scriptConfig.Triggers["prompt"] = []script.Step{
		{Response: &script.Response{Success: true}},
		{Event: json.RawMessage(`{"type":"message_start","message":{"role":"assistant","content":[]}}`)},
		{Event: json.RawMessage(`{"type":"message_update","usage":{"input":55,"output":8,"cacheRead":0,"cacheWrite":0,"totalTokens":63,"cost":{"input":0.00055,"output":0.00008,"cacheRead":0,"cacheWrite":0,"total":0.00063}},"assistantMessageEvent":{"type":"text_end","contentIndex":0}}`)},
		{Event: json.RawMessage(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"answer"}]}}`)},
		{Event: json.RawMessage(`{"type":"agent_settled"}`)},
	}
	setupFakePiEnv(t, scriptConfig)

	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
	})

	if result.Status != StatusCompleted {
		t.Fatalf("status = %q, error = %q", result.Status, result.Error)
	}
	if result.Usage == nil {
		t.Fatalf("usage = nil, want the completed run's usage")
	}
	if result.Usage.Input != 55 || result.Usage.Output != 8 || result.Usage.TotalTokens != 63 ||
		result.Usage.Cost.Input != 0.00055 || result.Usage.Cost.Output != 0.00008 || result.Usage.Cost.Total != 0.00063 {
		t.Fatalf("usage = %#v, want input 55 output 8 totalTokens 63 cost total 0.00063", result.Usage)
	}
	if result.Usage.CacheWrite1h != nil || result.Usage.Reasoning != nil {
		t.Fatalf("optional fields = %v/%v, want absent", result.Usage.CacheWrite1h, result.Usage.Reasoning)
	}
}

package pi

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/arasovic/pi-worker/internal/testutil/fakepi/script"
)

// messageUpdate builds one message_update event frame. Tests pass their own
// expected numbers as literals and never read them back out of this helper.
func messageUpdate(usage, subtype string) Event {
	return Event{
		Type: "message_update",
		Raw:  json.RawMessage(`{"type":"message_update","usage":` + usage + `,"assistantMessageEvent":{"type":"` + subtype + `"}}`),
	}
}

func TestUsageAccumulatorCumulativeFramesDoNotDoubleCount(t *testing.T) {
	// Every frame of one message carries the message's cumulative usage so
	// far, and only the terminal "done" frame is final. Reading every
	// frame would sum the growing intermediates on top of the final one
	// and report several times the true cost, so the expected numbers are
	// the done frame's, typed out again as literals below rather than
	// referenced from the frame that built them: the assertion must catch
	// both a one-frame reader and a four-frame summer.
	a := &usageAccumulator{}
	intermediates := []string{
		`{"input":10,"output":5,"cacheRead":2,"cacheWrite":1,"totalTokens":18,"cost":{"input":0.00010,"output":0.00005,"cacheRead":0.00002,"cacheWrite":0.00001,"total":0.00018}}`,
		`{"input":20,"output":15,"cacheRead":4,"cacheWrite":2,"totalTokens":41,"cost":{"input":0.00020,"output":0.00015,"cacheRead":0.00004,"cacheWrite":0.00002,"total":0.00041}}`,
		`{"input":30,"output":25,"cacheRead":6,"cacheWrite":3,"totalTokens":64,"cost":{"input":0.00030,"output":0.00025,"cacheRead":0.00006,"cacheWrite":0.00003,"total":0.00064}}`,
	}
	for _, usage := range intermediates {
		if err := a.OnEvent(messageUpdate(usage, "text_delta")); err != nil {
			t.Fatalf("OnEvent(intermediate) = %v, want nil", err)
		}
	}
	doneUsage := `{"input":45,"output":35,"cacheRead":10,"cacheWrite":5,"totalTokens":95,"cost":{"input":0.00045,"output":0.00035,"cacheRead":0.00010,"cacheWrite":0.00005,"total":0.00095}}`
	if err := a.OnEvent(messageUpdate(doneUsage, "done")); err != nil {
		t.Fatalf("OnEvent(done) = %v, want nil", err)
	}
	got := a.snapshot()
	want := &Usage{
		Input: 45, Output: 35, CacheRead: 10, CacheWrite: 5, TotalTokens: 95,
		Cost: UsageCost{Input: 0.00045, Output: 0.00035, CacheRead: 0.00010, CacheWrite: 0.00005, Total: 0.00095},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %#v, want the done frame's numbers %#v (not the sum of all four frames)", got, want)
	}
}

func TestUsageAccumulatorSumsAcrossMessages(t *testing.T) {
	// Two messages, two terminal frames: the total is the sum of both,
	// asserted as literals written in this test. The cost figures are
	// binary-exact so the float64 additions are exact and the literals
	// compare cleanly.
	a := &usageAccumulator{}
	first := `{"input":100,"output":20,"cacheRead":5,"cacheWrite":2,"totalTokens":127,"cost":{"input":0.5,"output":0.0625,"cacheRead":0.125,"cacheWrite":0.25,"total":1.0}}`
	second := `{"input":30,"output":10,"cacheRead":3,"cacheWrite":1,"totalTokens":44,"cost":{"input":0.25,"output":0.03125,"cacheRead":0.0625,"cacheWrite":0.5,"total":0.875}}`
	for _, usage := range []string{first, second} {
		if err := a.OnEvent(messageUpdate(usage, "done")); err != nil {
			t.Fatalf("OnEvent = %v, want nil", err)
		}
	}
	got := a.snapshot()
	want := &Usage{
		Input: 130, Output: 30, CacheRead: 8, CacheWrite: 3, TotalTokens: 171,
		Cost: UsageCost{Input: 0.75, Output: 0.09375, CacheRead: 0.1875, CacheWrite: 0.75, Total: 1.875},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %#v, want the sum %#v", got, want)
	}
}

func TestUsageAccumulatorUnterminatedStreamIsAbsent(t *testing.T) {
	// A stream that never reaches "done" or "error" reports nothing: the
	// field is absent, never a zero. An unterminated message is exactly
	// what a run that timed out or was killed mid-message leaves behind.
	a := &usageAccumulator{}
	for _, usage := range []string{
		`{"input":9,"output":2,"cacheRead":3,"cacheWrite":0,"totalTokens":14,"cost":{"input":0.00009,"output":0.00002,"cacheRead":0.00003,"cacheWrite":0,"total":0.00014}}`,
		`{"input":12,"output":4,"cacheRead":3,"cacheWrite":0,"totalTokens":19,"cost":{"input":0.00012,"output":0.00004,"cacheRead":0.00003,"cacheWrite":0,"total":0.00019}}`,
	} {
		if err := a.OnEvent(messageUpdate(usage, "text_delta")); err != nil {
			t.Fatalf("OnEvent = %v, want nil", err)
		}
	}
	if got := a.snapshot(); got != nil {
		t.Fatalf("snapshot = %#v, want nil: an unterminated stream must leave the field absent", got)
	}
}

func TestUsageAccumulatorMalformedUsageSkippedAndRunContinues(t *testing.T) {
	// A frame whose usage is missing, null, or not an object is skipped
	// silently. OnEvent must never return an error: per the EventHandler
	// contract an error is a protocol violation that fails the whole
	// client, so a measurement problem would fail a run that otherwise
	// worked. The valid terminal frame after it is still counted.
	a := &usageAccumulator{}
	for _, raw := range []string{
		`{"type":"message_update","usage":42,"assistantMessageEvent":{"type":"done"}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"done"}}`,
		`{"type":"message_update","usage":null,"assistantMessageEvent":{"type":"done"}}`,
		`{"type":"message_update","usage":"not an object","assistantMessageEvent":{"type":"done"}}`,
		`{"type":"message_update","usage":{"input":"not a number","output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":1,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"assistantMessageEvent":{"type":"done"}}`,
	} {
		if err := a.OnEvent(Event{Type: "message_update", Raw: json.RawMessage(raw)}); err != nil {
			t.Fatalf("OnEvent(malformed %q) = %v, want nil: a handler error fails the client", raw, err)
		}
	}
	valid := `{"input":7,"output":2,"cacheRead":0,"cacheWrite":0,"totalTokens":9,"cost":{"input":0.00007,"output":0.00002,"cacheRead":0,"cacheWrite":0,"total":0.00009}}`
	if err := a.OnEvent(messageUpdate(valid, "done")); err != nil {
		t.Fatalf("OnEvent(valid) = %v, want nil", err)
	}
	got := a.snapshot()
	want := &Usage{
		Input: 7, Output: 2, CacheRead: 0, CacheWrite: 0, TotalTokens: 9,
		Cost: UsageCost{Input: 0.00007, Output: 0.00002, CacheRead: 0, CacheWrite: 0, Total: 0.00009},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %#v, want only the valid frame's numbers %#v", got, want)
	}
}

func TestUsageAccumulatorErrorFrameTerminatesAndCounts(t *testing.T) {
	// assistantMessageEvent.type "error" terminates a message too: a
	// message that ended in a model error still spent money, and its
	// terminal frame's usage counts like a "done" frame's.
	a := &usageAccumulator{}
	usage := `{"input":13,"output":4,"cacheRead":6,"cacheWrite":1,"totalTokens":24,"cost":{"input":0.00013,"output":0.00004,"cacheRead":0.00006,"cacheWrite":0.00001,"total":0.00024}}`
	if err := a.OnEvent(messageUpdate(usage, "error")); err != nil {
		t.Fatalf("OnEvent(error) = %v, want nil", err)
	}
	got := a.snapshot()
	want := &Usage{
		Input: 13, Output: 4, CacheRead: 6, CacheWrite: 1, TotalTokens: 24,
		Cost: UsageCost{Input: 0.00013, Output: 0.00004, CacheRead: 0.00006, CacheWrite: 0.00001, Total: 0.00024},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %#v, want the error frame's numbers %#v", got, want)
	}
}

func TestUsageAccumulatorOptionalFieldsSumWhenReported(t *testing.T) {
	// cacheWrite1h and reasoning are omitted when no message reported
	// them, and summed when any did.
	a := &usageAccumulator{}
	withOptional := `{"input":40,"output":9,"cacheRead":0,"cacheWrite":1,"cacheWrite1h":5,"reasoning":7,"totalTokens":50,"cost":{"input":0.0004,"output":0.00009,"cacheRead":0,"cacheWrite":0.00001,"total":0.0005}}`
	withoutOptional := `{"input":30,"output":7,"cacheRead":0,"cacheWrite":0,"totalTokens":37,"cost":{"input":0.0003,"output":0.00007,"cacheRead":0,"cacheWrite":0,"total":0.00037}}`
	if err := a.OnEvent(messageUpdate(withOptional, "done")); err != nil {
		t.Fatalf("OnEvent = %v, want nil", err)
	}
	if err := a.OnEvent(messageUpdate(withoutOptional, "done")); err != nil {
		t.Fatalf("OnEvent = %v, want nil", err)
	}
	got := a.snapshot()
	if got == nil {
		t.Fatalf("snapshot = nil, want present usage")
	}
	if got.CacheWrite1h == nil || *got.CacheWrite1h != 5 {
		t.Fatalf("cacheWrite1h = %v, want 5: reported once, summed once", got.CacheWrite1h)
	}
	if got.Reasoning == nil || *got.Reasoning != 7 {
		t.Fatalf("reasoning = %v, want 7", got.Reasoning)
	}
	if got.Input != 70 || got.Output != 16 || got.TotalTokens != 87 {
		t.Fatalf("totals = %#v, want input 70 output 16 totalTokens 87", got)
	}

	// A message stream that never reported the optional fields omits them
	// from the document, never emits a zero.
	b := &usageAccumulator{}
	if err := b.OnEvent(messageUpdate(withoutOptional, "done")); err != nil {
		t.Fatalf("OnEvent = %v, want nil", err)
	}
	gotB := b.snapshot()
	if gotB.CacheWrite1h != nil || gotB.Reasoning != nil {
		t.Fatalf("optional fields = %v/%v, want absent", gotB.CacheWrite1h, gotB.Reasoning)
	}
	data, err := json.Marshal(gotB)
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

func TestUsageAccumulatorMeasuredZeroIsNotAbsent(t *testing.T) {
	// A terminal frame that reports a free message (all zeros) is a
	// measurement: the field must be present with zeros, never nil.
	a := &usageAccumulator{}
	zero := `{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}`
	if err := a.OnEvent(messageUpdate(zero, "done")); err != nil {
		t.Fatalf("OnEvent = %v, want nil", err)
	}
	got := a.snapshot()
	if got == nil {
		t.Fatalf("snapshot = nil, want present all-zero usage: a zero says measured-and-free, absence says never measured")
	}
	want := &Usage{Cost: UsageCost{}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %#v, want all-zero %#v", got, want)
	}
}

func TestWorkerReportsUsageOnFailedRun(t *testing.T) {
	// A run that fails after its message stream still spent money, and the
	// document is what records the spend: the result carries both the
	// failure status and the usage.
	scriptConfig := happyPathScript("unused")
	scriptConfig.Triggers["prompt"] = []script.Step{
		{Response: &script.Response{Success: true}},
		{Event: json.RawMessage(`{"type":"message_update","usage":{"input":900,"output":60,"cacheRead":100,"cacheWrite":4,"cacheWrite1h":2,"reasoning":120,"totalTokens":1084,"cost":{"input":0.009,"output":0.003,"cacheRead":0.001,"cacheWrite":0.0002,"total":0.0132}},"assistantMessageEvent":{"type":"done"}}`)},
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
	// every other status, so a successful run carries the same shape.
	scriptConfig := happyPathScript("answer")
	scriptConfig.Triggers["prompt"] = []script.Step{
		{Response: &script.Response{Success: true}},
		{Event: json.RawMessage(`{"type":"message_update","usage":{"input":55,"output":8,"cacheRead":0,"cacheWrite":0,"totalTokens":63,"cost":{"input":0.00055,"output":0.00008,"cacheRead":0,"cacheWrite":0,"total":0.00063}},"assistantMessageEvent":{"type":"done"}}`)},
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

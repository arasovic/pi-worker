package pi

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/testutil/fakepi/script"
)

// activityEvent builds one Pi event with the given type and raw body.
func activityEvent(eventType, body string) Event {
	return Event{Type: eventType, Raw: json.RawMessage(body)}
}

// collectActivity returns a fake clock and an observer that appends every
// report it is given, so a test can drive the handler's clock explicitly.
func collectActivity() (*fakeClock, *[]Activity) {
	clock := &fakeClock{t: time.Unix(0, 0).UTC()}
	observed := &[]Activity{}
	return clock, observed
}

// TestActivityAccumulatorCountsToolStartsAndProjectsNames verifies that
// every tool start is counted once and that the reported name is the fixed
// allowlist projection, never the raw Pi-controlled value.
func TestActivityAccumulatorCountsToolStartsAndProjectsNames(t *testing.T) {
	clock, observed := collectActivity()
	a := &activityAccumulator{workerID: 2, now: clock.now, observe: func(workerID int, activity Activity) {
		if workerID != 2 {
			t.Fatalf("observer worker id = %d, want 2", workerID)
		}
		*observed = append(*observed, activity)
	}}

	// A known tool, with an arguments field the projection must ignore.
	a.OnEvent(activityEvent("tool_execution_start",
		`{"toolCallId":"c1","toolName":"bash","arguments":{"command":"secret"}}`))
	if len(*observed) != 1 {
		t.Fatalf("observations = %d, want one after a tool start", len(*observed))
	}
	if got := (*observed)[0]; got.ToolCalls != 1 || got.LastTool != "bash" {
		t.Fatalf("activity = %+v, want one bash tool call", got)
	}
	if got := (*observed)[0].LastEventAt; !got.Equal(time.Unix(0, 0).UTC()) {
		t.Fatalf("lastEventAt = %v, want the fake clock instant", got)
	}

	// An unknown tool name reports exactly the value toolName gives.
	a.OnEvent(activityEvent("tool_execution_start", `{"toolCallId":"c2","toolName":"rm -rf /"}`))
	if len(*observed) != 2 {
		t.Fatalf("observations = %d, want two after the second tool start", len(*observed))
	}
	wantUnknown := toolName("rm -rf /")
	if got := (*observed)[1]; got.ToolCalls != 2 || got.LastTool != wantUnknown || got.LastTool != unknownName {
		t.Fatalf("activity = %+v, want toolCalls 2 and the fixed unknown projection %q", got, wantUnknown)
	}
}

// TestActivityAccumulatorThrottlesNonToolEvents verifies the interval rule:
// the first non-tool event reports, a second inside the interval is silent,
// and a later event after the interval reports the latest values.
func TestActivityAccumulatorThrottlesNonToolEvents(t *testing.T) {
	clock, observed := collectActivity()
	a := &activityAccumulator{workerID: 1, now: clock.now, observe: func(_ int, activity Activity) {
		*observed = append(*observed, activity)
	}}

	a.OnEvent(activityEvent("agent_start", `{}`))
	if len(*observed) != 1 {
		t.Fatalf("observations = %d, want the first report immediately", len(*observed))
	}
	if got := (*observed)[0].LastEventAt; !got.Equal(time.Unix(0, 0).UTC()) {
		t.Fatalf("first lastEventAt = %v, want t=0", got)
	}

	// Inside the interval: silent, but the retained event time advances.
	clock.advance(5 * time.Second)
	a.OnEvent(activityEvent("message_update", `{}`))
	if len(*observed) != 1 {
		t.Fatalf("observations = %d, want no second report inside %v", len(*observed), activityInterval)
	}

	// At the interval boundary the next event reports again.
	clock.advance(5 * time.Second)
	a.OnEvent(activityEvent("agent_start", `{}`))
	if len(*observed) != 2 {
		t.Fatalf("observations = %d, want a second report after %v", len(*observed), activityInterval)
	}
	if got := (*observed)[1].LastEventAt; !got.Equal(time.Unix(10, 0).UTC()) {
		t.Fatalf("second lastEventAt = %v, want the latest t=10", got)
	}
}

// TestActivityAccumulatorReportsEveryToolStart verifies that two tool
// starts one second apart are both reported, even though they are inside
// the non-tool interval: a tool start is bounded by a model round trip.
func TestActivityAccumulatorReportsEveryToolStart(t *testing.T) {
	clock, observed := collectActivity()
	a := &activityAccumulator{workerID: 1, now: clock.now, observe: func(_ int, activity Activity) {
		*observed = append(*observed, activity)
	}}

	a.OnEvent(activityEvent("tool_execution_start", `{"toolName":"bash"}`))
	clock.advance(time.Second)
	a.OnEvent(activityEvent("tool_execution_start", `{"toolName":"read"}`))

	if len(*observed) != 2 {
		t.Fatalf("observations = %d, want two tool-start reports", len(*observed))
	}
	if got := (*observed)[1]; got.ToolCalls != 2 || got.LastTool != "read" {
		t.Fatalf("second activity = %+v, want the second tool", got)
	}
}

// TestActivityAccumulatorNilObserverIsInert verifies that a nil observer
// makes the handler do nothing observable and never fail.
func TestActivityAccumulatorNilObserverIsInert(t *testing.T) {
	clock, _ := collectActivity()
	a := &activityAccumulator{workerID: 1, now: clock.now}
	for _, event := range []Event{
		activityEvent("agent_start", `{}`),
		activityEvent("tool_execution_start", `{"toolName":"bash"}`),
	} {
		if err := a.OnEvent(event); err != nil {
			t.Fatalf("OnEvent(%q) = %v, want nil", event.Type, err)
		}
	}
}

// TestWorkerReportsActivityEndToEnd drives the full worker against the fake
// Pi: the prompt emits one tool_execution_start before the answer, and the
// observer must see the projected tool and its count.
func TestWorkerReportsActivityEndToEnd(t *testing.T) {
	cfg := happyPathScript("activity answer")
	cfg.Triggers["prompt"] = []script.Step{
		{Response: &script.Response{Success: true}},
		{Event: json.RawMessage(`{"type":"agent_start"}`)},
		{Event: json.RawMessage(`{"type":"tool_execution_start","toolCallId":"c1","toolName":"bash"}`)},
		{Event: json.RawMessage(`{"type":"tool_execution_end","toolCallId":"c1","toolName":"bash","isError":false}`)},
		{Event: json.RawMessage(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"The answer is 42."}]}}`)},
		{Event: json.RawMessage(`{"type":"turn_end","message":{},"toolResults":[]}`)},
		{Event: json.RawMessage(`{"type":"agent_end","messages":[],"willRetry":false}`)},
		{Event: json.RawMessage(`{"type":"agent_settled"}`)},
	}
	setupFakePiEnv(t, cfg)

	var observed []Activity
	result := New(fakePiBin).Run(context.Background(), WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "go",
		Workspace: t.TempDir(),
		OnActivity: func(workerID int, activity Activity) {
			observed = append(observed, activity)
		},
	})

	if result.Status != StatusCompleted {
		t.Fatalf("result = %+v, want completed", result)
	}
	foundTool := false
	for _, activity := range observed {
		if activity.ToolCalls == 1 && activity.LastTool == "bash" {
			foundTool = true
		}
	}
	if !foundTool {
		t.Fatalf("activity observations = %+v, want one with toolCalls 1 and lastTool bash", observed)
	}
}

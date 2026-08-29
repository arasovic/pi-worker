package cli

import (
	"encoding/json"
	"testing"

	"github.com/arasovic/pi-worker/internal/testutil/fakepi/script"
)

// TestRunJSONCarriesPartialTextWhenRunEndsWithoutFinalText drives a real
// worker against a scripted fakepi stream that emits message_start and
// real text_delta frames and then never emits agent_settled: the run is
// killed by the timeout while the model is mid-answer, exactly the shape
// of the measured long-run kills. The deltas must land in the run --json
// document as partialExplanation, and explanation must stay absent —
// truncated text under the completed-run name would turn an interrupted
// run into a complete-looking answer. The raw frames travel the same
// path real Pi frames do: fakepi writes them verbatim, the client
// delivers them to the worker's handler, and the document carries what
// the handler accumulated.
func TestRunJSONCarriesPartialTextWhenRunEndsWithoutFinalText(t *testing.T) {
	installRealFakePiWorker(t)
	setupFakePiScript(t, &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}},
		},
		"set_model": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"provider":"acme","id":"m-1"}`)}},
		},
		"prompt": {
			{Response: &script.Response{Success: true}},
			{Event: json.RawMessage(`{"type":"message_start","message":{"role":"assistant","content":[]}}`)},
			{Event: json.RawMessage(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"The answer"}}`)},
			{Event: json.RawMessage(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":" is"}}`)},
			{Event: json.RawMessage(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":" 42"}}`)},
			// No agent_settled ever: fakepi idles, the run deadline fires,
			// and the worker reports what streamed before the kill.
			{SleepMS: 10000},
		},
	}})

	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--json", "--timeout", "3s"}, "")
	if code != 7 {
		t.Fatalf("exit = %d, want 7 (timed out); stderr = %q", code, stderr)
	}
	document := decodeJSONObject(t, stdout)
	workers, ok := document["workers"].([]any)
	if !ok || len(workers) != 1 {
		t.Fatalf("workers = %#v, want one worker", document["workers"])
	}
	worker, ok := workers[0].(map[string]any)
	if !ok {
		t.Fatalf("worker = %#v, want object", workers[0])
	}
	if _, present := worker["explanation"]; present {
		t.Fatalf("explanation = %#v, want absent on a run that never settled", worker["explanation"])
	}
	// The scripted frames' deltas, as literals: the field must carry their
	// concatenation in stream order.
	if partial, ok := worker["partialExplanation"].(string); !ok || partial != "The answer is 42" {
		t.Fatalf("partialExplanation = %#v, want %q", worker["partialExplanation"], "The answer is 42")
	}
}

// TestRunJSONOmitsPartialTextWhenRunCompletes drives the same stream to a
// normal settlement: agent_settled arrives, get_last_assistant_text
// returns the final text, and the run completes. The scripted stream
// deliberately carries real text_delta frames, so the accumulator holds
// text when the result is built — the field's absence must be caused by
// the Explanation == "" condition and nothing else. A stream without
// deltas would leave the accumulator empty and make this test pass
// whether the guard works or not.
func TestRunJSONOmitsPartialTextWhenRunCompletes(t *testing.T) {
	installRealFakePiWorker(t)
	setupFakePiScript(t, &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}},
		},
		"set_model": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"provider":"acme","id":"m-1"}`)}},
		},
		"prompt": {
			{Response: &script.Response{Success: true}},
			{Event: json.RawMessage(`{"type":"message_start","message":{"role":"assistant","content":[]}}`)},
			{Event: json.RawMessage(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"The answer"}}`)},
			{Event: json.RawMessage(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":" is 42"}}`)},
			{Event: json.RawMessage(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"The answer is 42"}]}}`)},
			{Event: json.RawMessage(`{"type":"agent_settled"}`)},
		},
		"get_last_assistant_text": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"text":"The answer is 42"}`)}},
		},
	}})

	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/m-1", "--task", "go", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	document := decodeJSONObject(t, stdout)
	workers, ok := document["workers"].([]any)
	if !ok || len(workers) != 1 {
		t.Fatalf("workers = %#v, want one worker", document["workers"])
	}
	worker, ok := workers[0].(map[string]any)
	if !ok {
		t.Fatalf("worker = %#v, want object", workers[0])
	}
	if explanation, ok := worker["explanation"].(string); !ok || explanation != "The answer is 42" {
		t.Fatalf("explanation = %#v, want the final text %q", worker["explanation"], "The answer is 42")
	}
	if _, present := worker["partialExplanation"]; present {
		t.Fatalf("partialExplanation = %#v, want absent on a completed run", worker["partialExplanation"])
	}
}

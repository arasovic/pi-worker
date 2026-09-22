package cli

import (
	"encoding/json"
	"testing"

	"github.com/arasovic/pi-worker/internal/testutil/fakepi/script"
)

// TestRunJSONCarriesWorkerUsageEndToEnd drives a real worker against a
// scripted fakepi message_update stream and asserts Pi's usage lands in
// the run --json document, field names and values unchanged. The raw
// frames travel the same path real Pi frames do: fakepi writes them
// verbatim, the client delivers them to the worker's handler, and the
// document carries what the handler summed. The scripted frame uses the
// real vocabulary — a message_start/message_end bounded message whose
// text_end frame carries the numbers — so the path exercised is the path
// a real run takes, not a fallback.
func TestRunJSONCarriesWorkerUsageEndToEnd(t *testing.T) {
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
			{Event: json.RawMessage(`{"type":"message_update","usage":{"input":1200,"output":340,"cacheRead":80,"cacheWrite":6,"cacheWrite1h":3,"reasoning":200,"totalTokens":1829,"cost":{"input":0.012,"output":0.017,"cacheRead":0.0008,"cacheWrite":0.0003,"total":0.0301}},"assistantMessageEvent":{"type":"text_end","contentIndex":0}}`)},
			{Event: json.RawMessage(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`)},
			{Event: json.RawMessage(`{"type":"agent_settled"}`)},
		},
		"get_last_assistant_text": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"text":"done"}`)}},
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
	if _, present := worker["cacheWarmUsage"]; present {
		t.Fatalf("worker carries cacheWarmUsage = %#v, want the key absent when no warm request was reported", worker["cacheWarmUsage"])
	}
	usage, ok := worker["usage"].(map[string]any)
	if !ok {
		t.Fatalf("worker usage = %#v, want object carrying Pi's usage", worker["usage"])
	}
	assertExactJSONKeys(t, usage, "cacheRead", "cacheWrite", "cacheWrite1h", "cost", "input", "output", "reasoning", "totalTokens")
	// The scripted frame's values, as literals: JSON numbers decode to
	// float64, so the ints are compared in that type.
	if usage["input"] != float64(1200) || usage["output"] != float64(340) || usage["cacheRead"] != float64(80) || usage["cacheWrite"] != float64(6) {
		t.Fatalf("usage counters = %#v, want input 1200 output 340 cacheRead 80 cacheWrite 6", usage)
	}
	if usage["cacheWrite1h"] != float64(3) || usage["reasoning"] != float64(200) || usage["totalTokens"] != float64(1829) {
		t.Fatalf("usage counters = %#v, want cacheWrite1h 3 reasoning 200 totalTokens 1829", usage)
	}
	cost, ok := usage["cost"].(map[string]any)
	if !ok {
		t.Fatalf("cost = %#v, want object", usage["cost"])
	}
	assertExactJSONKeys(t, cost, "cacheRead", "cacheWrite", "input", "output", "total")
	if cost["input"] != 0.012 || cost["output"] != 0.017 || cost["cacheRead"] != 0.0008 || cost["cacheWrite"] != 0.0003 || cost["total"] != 0.0301 {
		t.Fatalf("cost = %#v, want input 0.012 output 0.017 cacheRead 0.0008 cacheWrite 0.0003 total 0.0301", cost)
	}
}

// TestRunJSONCarriesWorkerCacheWarmUsageEndToEnd drives a real worker
// against a scripted fakepi stream that appends one cache-warm frame after
// the assistant message. The warm figure must land in the worker's own
// cacheWarmUsage object, field names and values unchanged, while usage
// still carries only the message's numbers. The frame travels the same
// verbatim path a real Pi warm request's entry_appended frame takes.
func TestRunJSONCarriesWorkerCacheWarmUsageEndToEnd(t *testing.T) {
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
			{Event: json.RawMessage(`{"type":"message_update","usage":{"input":1200,"output":340,"cacheRead":80,"cacheWrite":6,"cacheWrite1h":3,"reasoning":200,"totalTokens":1829,"cost":{"input":0.012,"output":0.017,"cacheRead":0.0008,"cacheWrite":0.0003,"total":0.0301}},"assistantMessageEvent":{"type":"text_end","contentIndex":0}}`)},
			{Event: json.RawMessage(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`)},
			{Event: json.RawMessage(`{"type":"entry_appended","entry":{"type":"usage","id":"a1b2c3d4","parentId":"e5f6a7b8","timestamp":"2026-09-23T10:00:00.000Z","kind":"cache_warm","provider":"acme","model":"m-1","usage":{"input":245,"output":1,"cacheRead":2048,"cacheWrite":0,"totalTokens":2294,"cost":{"input":0.5,"output":0.25,"cacheRead":0.125,"cacheWrite":0,"total":0.875}}}}`)},
			{Event: json.RawMessage(`{"type":"agent_settled"}`)},
		},
		"get_last_assistant_text": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"text":"done"}`)}},
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

	usage, ok := worker["usage"].(map[string]any)
	if !ok {
		t.Fatalf("worker usage = %#v, want object carrying only the message's numbers", worker["usage"])
	}
	if usage["input"] != float64(1200) || usage["output"] != float64(340) || usage["cacheRead"] != float64(80) || usage["cacheWrite"] != float64(6) || usage["cacheWrite1h"] != float64(3) || usage["reasoning"] != float64(200) || usage["totalTokens"] != float64(1829) {
		t.Fatalf("usage counters = %#v, want only the message's input 1200 output 340 cacheRead 80 cacheWrite 6 cacheWrite1h 3 reasoning 200 totalTokens 1829", usage)
	}

	cacheWarm, ok := worker["cacheWarmUsage"].(map[string]any)
	if !ok {
		t.Fatalf("worker cacheWarmUsage = %#v, want object carrying the warm request's numbers", worker["cacheWarmUsage"])
	}
	assertExactJSONKeys(t, cacheWarm, "cacheRead", "cacheWrite", "cost", "input", "output", "totalTokens")
	if cacheWarm["input"] != float64(245) || cacheWarm["output"] != float64(1) || cacheWarm["cacheRead"] != float64(2048) || cacheWarm["cacheWrite"] != float64(0) || cacheWarm["totalTokens"] != float64(2294) {
		t.Fatalf("cacheWarmUsage counters = %#v, want input 245 output 1 cacheRead 2048 cacheWrite 0 totalTokens 2294", cacheWarm)
	}
	cost, ok := cacheWarm["cost"].(map[string]any)
	if !ok {
		t.Fatalf("cacheWarmUsage cost = %#v, want object", cacheWarm["cost"])
	}
	assertExactJSONKeys(t, cost, "cacheRead", "cacheWrite", "input", "output", "total")
	if cost["input"] != 0.5 || cost["output"] != 0.25 || cost["cacheRead"] != 0.125 || cost["cacheWrite"] != float64(0) || cost["total"] != 0.875 {
		t.Fatalf("cacheWarmUsage cost = %#v, want input 0.5 output 0.25 cacheRead 0.125 cacheWrite 0 total 0.875", cost)
	}
}

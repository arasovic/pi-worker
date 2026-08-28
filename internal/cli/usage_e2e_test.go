package cli

import (
	"encoding/json"
	"testing"

	"github.com/arasovic/pi-worker/internal/testutil/fakepi/script"
)

// TestRunJSONCarriesWorkerUsageEndToEnd drives a real worker against a
// scripted fakepi message_update stream and asserts Pi's usage lands in
// the run --json document, field names and values unchanged. The raw
// frame travels the same path a real Pi frame does: fakepi writes it
// verbatim, the client delivers it to the worker's handler, and the
// document carries what the handler summed.
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
			{Event: json.RawMessage(`{"type":"message_update","usage":{"input":1200,"output":340,"cacheRead":80,"cacheWrite":6,"cacheWrite1h":3,"reasoning":200,"totalTokens":1829,"cost":{"input":0.012,"output":0.017,"cacheRead":0.0008,"cacheWrite":0.0003,"total":0.0301}},"assistantMessageEvent":{"type":"done"}}`)},
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

package pi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"pi-worker/internal/testutil/fakepi/script"
)

// recordingHandler records every delivered event.
type recordingHandler struct {
	events []Event
}

func (h *recordingHandler) OnEvent(event Event) error {
	h.events = append(h.events, event)
	return nil
}

func (h *recordingHandler) types() []string {
	types := make([]string, len(h.events))
	for i, event := range h.events {
		types[i] = event.Type
	}
	return types
}

type closedPipeWriter struct{}

func (closedPipeWriter) Write(_ []byte) (int, error) {
	return 0, io.ErrClosedPipe
}

func TestClientCorrelatesInterleavedResponsesAndEvents(t *testing.T) {
	script := &script.Script{Triggers: map[string][]script.Step{
		"prompt": {
			{Event: json.RawMessage(`{"type":"agent_start"}`)},
			{Event: json.RawMessage(`{"type":"agent_end","messages":[],"willRetry":false}`)},
			{Response: &script.Response{Success: true}},
			{Event: json.RawMessage(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"hi"}}`)},
			{Event: json.RawMessage(`{"type":"agent_settled"}`)},
		},
		"get_last_assistant_text": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"text":"final answer"}`)}},
		},
	}}
	proc := startScriptedPi(t, script)
	handler := &recordingHandler{}
	client := NewClient(proc.Stdin(), proc.Stdout(), handler, nil)

	if err := client.Prompt(context.Background(), "hello"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if err := client.WaitSettled(context.Background()); err != nil {
		t.Fatalf("wait settled: %v", err)
	}
	text, err := client.GetLastAssistantText(context.Background())
	if err != nil {
		t.Fatalf("get last assistant text: %v", err)
	}
	if text != "final answer" {
		t.Fatalf("text = %q", text)
	}
	want := []string{"agent_start", "agent_end", "message_update", "agent_settled"}
	if !slices.Equal(handler.types(), want) {
		t.Fatalf("events = %v, want %v", handler.types(), want)
	}
}

func TestClientAgentEndIsNotTerminal(t *testing.T) {
	// WaitSettled must consume every frame through agent_settled. If
	// agent_end were treated as terminal, the event snapshot taken right
	// after WaitSettled would stop at agent_end instead.
	script := &script.Script{Triggers: map[string][]script.Step{
		"prompt": {
			{Response: &script.Response{Success: true}},
			{Event: json.RawMessage(`{"type":"agent_end","messages":[],"willRetry":false}`)},
			{Event: json.RawMessage(`{"type":"agent_settled"}`)},
		},
		"get_last_assistant_text": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"text":"settled text"}`)}},
		},
	}}
	proc := startScriptedPi(t, script)
	handler := &recordingHandler{}
	client := NewClient(proc.Stdin(), proc.Stdout(), handler, nil)

	if err := client.Prompt(context.Background(), "hello"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if err := client.WaitSettled(context.Background()); err != nil {
		t.Fatalf("wait settled: %v", err)
	}
	want := []string{"agent_end", "agent_settled"}
	if !slices.Equal(handler.types(), want) {
		t.Fatalf("events when settled = %v, want %v", handler.types(), want)
	}
	text, err := client.GetLastAssistantText(context.Background())
	if err != nil {
		t.Fatalf("final text after settled: %v", err)
	}
	if text != "settled text" {
		t.Fatalf("text = %q", text)
	}
	if !slices.Equal(handler.types(), want) {
		t.Fatalf("events changed after settlement: %v", handler.types())
	}
}

func TestClientWaitSettledIgnoresCatalogSettledEvent(t *testing.T) {
	// A settlement reported while catalog setup is in flight must not satisfy
	// the later prompt settlement wait. With no post-prompt settlement, an
	// already-cancelled wait must return the context error, not stale success.
	scriptConfig := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Event: json.RawMessage(`{"type":"agent_settled"}`)},
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}},
		},
		"set_model": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"provider":"acme","id":"m-1"}`)}},
		},
		"prompt": {
			{Response: &script.Response{Success: true}},
		},
	}}
	proc := startScriptedPi(t, scriptConfig)
	handler := &recordingHandler{}
	client := NewClient(proc.Stdin(), proc.Stdout(), handler, nil)

	if _, err := client.GetAvailableModels(context.Background()); err != nil {
		t.Fatalf("get available models: %v", err)
	}
	if err := client.SetModel(context.Background(), "acme", "m-1"); err != nil {
		t.Fatalf("set model: %v", err)
	}
	if err := client.Prompt(context.Background(), "hello"); err != nil {
		t.Fatalf("prompt: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.WaitSettled(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitSettled = %v, want context.Canceled", err)
	}
	want := []string{"agent_settled"}
	if !slices.Equal(handler.types(), want) {
		t.Fatalf("events = %v, want %v", handler.types(), want)
	}
}

// TestClientTransportEOFAfterPromptBeforeSettledIdentifiesRPCStream is the
// regression for transport failures naming the Pi RPC stream: a transport
// EOF after prompt acceptance and before agent_settled must surface as a
// user-visible error that identifies the pi rpc stream, while staying
// generic and secret-safe: no raw stream detail and no prompt content may
// leak into the message.
func TestClientTransportEOFAfterPromptBeforeSettledIdentifiesRPCStream(t *testing.T) {
	script := &script.Script{Triggers: map[string][]script.Step{
		"prompt": {
			{Response: &script.Response{Success: true}},
			{Exit: true},
		},
	}}
	proc := startScriptedPi(t, script)
	client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

	const promptText = "SECRET-PROMPT-9f2c"
	if err := client.Prompt(context.Background(), promptText); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	err := client.WaitSettled(context.Background())
	if err == nil {
		t.Fatalf("WaitSettled after transport EOF returned nil, want a transport error")
	}
	var transportErr *transportError
	if !errors.As(err, &transportErr) {
		t.Fatalf("WaitSettled = %v, want *transportError", err)
	}
	message := err.Error()
	if !strings.Contains(message, "pi rpc stream") {
		t.Fatalf("WaitSettled error = %q, want it to identify the pi rpc stream", message)
	}
	if strings.Contains(message, promptText) || strings.Contains(message, "EOF") {
		t.Fatalf("WaitSettled error leaked transport detail: %q", message)
	}
}

func TestClientUnknownResponseIDFails(t *testing.T) {
	script := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{ID: "wrong-1", Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}},
		},
	}}
	proc := startScriptedPi(t, script)
	client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

	_, err := client.GetAvailableModels(context.Background())
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) {
		t.Fatalf("err = %v, want *ProtocolError", err)
	}
	if !strings.Contains(err.Error(), "unknown request id") {
		t.Fatalf("error = %q", err)
	}
}

func TestClientResponseCommandMismatchFails(t *testing.T) {
	script := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Command: "prompt", Success: true, Data: json.RawMessage(`{"models":[]}`)}},
		},
	}}
	proc := startScriptedPi(t, script)
	client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

	_, err := client.GetAvailableModels(context.Background())
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) {
		t.Fatalf("err = %v, want *ProtocolError", err)
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %q", err)
	}
}

func TestClientMalformedFrameFails(t *testing.T) {
	script := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {{Raw: "{not json"}},
	}}
	proc := startScriptedPi(t, script)
	client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

	_, err := client.GetAvailableModels(context.Background())
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) {
		t.Fatalf("err = %v, want *ProtocolError", err)
	}
}

func TestClientEmptyFrameFails(t *testing.T) {
	script := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {{Raw: ""}},
	}}
	proc := startScriptedPi(t, script)
	client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

	_, err := client.GetAvailableModels(context.Background())
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) {
		t.Fatalf("err = %v, want *ProtocolError", err)
	}
}

func TestClientOversizedFrameFails(t *testing.T) {
	script := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {{Raw: strings.Repeat("x", MaxFrameBytes+16)}},
	}}
	proc := startScriptedPi(t, script)
	client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

	_, err := client.GetAvailableModels(context.Background())
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) {
		t.Fatalf("err = %v, want *ProtocolError", err)
	}
	if !strings.Contains(err.Error(), "oversized") {
		t.Fatalf("error = %q", err)
	}
}

func TestClientStartupReadFailureIsReadinessError(t *testing.T) {
	t.Setenv("FAKEPI_STDERR", "FAKEPI-STDERR-SECRET-4e3c")
	proc := startScriptedPi(t, &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {{Exit: true}},
	}})
	client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

	_, err := client.GetAvailableModels(context.Background())
	var readinessErr *ReadinessError
	if !errors.As(err, &readinessErr) {
		t.Fatalf("err = %v, want *ReadinessError", err)
	}
	message := err.Error()
	lower := strings.ToLower(message)
	for _, want := range []string{"0.84.1", "compatib", "provider", "login"} {
		if !strings.Contains(lower, strings.ToLower(want)) {
			t.Fatalf("error = %q, want detail %q", message, want)
		}
	}
	if strings.Contains(message, "FAKEPI-STDERR-SECRET-4e3c") {
		t.Fatalf("error exposed child stderr: %q", message)
	}
}

func TestClientStartupWriteFailureIsReadinessError(t *testing.T) {
	client := NewClient(closedPipeWriter{}, strings.NewReader(""), nil, nil)

	_, err := client.GetAvailableModels(context.Background())
	var readinessErr *ReadinessError
	if !errors.As(err, &readinessErr) {
		t.Fatalf("err = %v, want *ReadinessError", err)
	}
	message := err.Error()
	lower := strings.ToLower(message)
	for _, want := range []string{"0.84.1", "compatib", "provider", "login"} {
		if !strings.Contains(lower, strings.ToLower(want)) {
			t.Fatalf("error = %q, want detail %q", message, want)
		}
	}
}

func TestClientGetAvailableModelsRejectsInvalidSuccessContainers(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "missing data", data: ""},
		{name: "null data", data: "null"},
		{name: "missing models", data: `{}`},
		{name: "null models", data: `{"models":null}`},
		{name: "data is array", data: `[]`},
		{name: "data is string", data: `"catalog"`},
		{name: "data is number", data: `7`},
		{name: "models is object", data: `{"models":{}}`},
		{name: "models is string", data: `{"models":"x"}`},
		{name: "models is number", data: `{"models":7}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			script := &script.Script{Triggers: map[string][]script.Step{
				"get_available_models": {{Response: &script.Response{Success: true, Data: json.RawMessage(test.data)}}},
			}}
			proc := startScriptedPi(t, script)
			client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

			_, err := client.GetAvailableModels(context.Background())
			var protocolErr *ProtocolError
			if !errors.As(err, &protocolErr) {
				t.Fatalf("err = %v, want *ProtocolError", err)
			}
		})
	}
}

func TestClientGetAvailableModelsExplicitEmptyArrayIsValidCatalog(t *testing.T) {
	script := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[]}`)}}},
	}}
	proc := startScriptedPi(t, script)
	client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

	models, err := client.GetAvailableModels(context.Background())
	if err != nil {
		t.Fatalf("get available models: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("models = %v, want empty catalog", models)
	}
}

func TestClientGetAvailableModelsRejectsMalformedSelectors(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		id       string
	}{
		{name: "slash in provider", provider: "ac/me", id: "model"},
		{name: "slash in id", provider: "acme", id: "mo/del"},
		{name: "colon", provider: "acme", id: "model:thinking"},
		{name: "ASCII whitespace", provider: "acme", id: "mo del"},
		{name: "Unicode whitespace", provider: "acme", id: "mo\u00a0del"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data, err := json.Marshal(map[string]any{"models": []ModelProjection{{Provider: test.provider, ID: test.id}}})
			if err != nil {
				t.Fatalf("marshal catalog: %v", err)
			}
			script := &script.Script{Triggers: map[string][]script.Step{
				"get_available_models": {{Response: &script.Response{Success: true, Data: data}}},
			}}
			proc := startScriptedPi(t, script)
			_, err = NewClient(proc.Stdin(), proc.Stdout(), nil, nil).GetAvailableModels(context.Background())
			var protocolErr *ProtocolError
			if !errors.As(err, &protocolErr) {
				t.Fatalf("error = %v, want *ProtocolError", err)
			}
			if strings.Contains(err.Error(), test.provider) || strings.Contains(err.Error(), test.id) {
				t.Fatalf("error leaked malformed selector: %q", err)
			}
		})
	}
}

func TestClientGetAvailableModelsAcceptsDashedDottedSelector(t *testing.T) {
	script := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"open-ai","id":"gpt.4o-mini"}]}`)}}},
	}}
	proc := startScriptedPi(t, script)
	models, err := NewClient(proc.Stdin(), proc.Stdout(), nil, nil).GetAvailableModels(context.Background())
	if err != nil {
		t.Fatalf("GetAvailableModels error: %v", err)
	}
	want := []ModelProjection{{Provider: "open-ai", ID: "gpt.4o-mini"}}
	if !slices.Equal(models, want) {
		t.Fatalf("models = %#v, want %#v", models, want)
	}
}

func TestClientSuccessFalseMapsToTypedErrors(t *testing.T) {
	tests := []struct {
		name  string
		kind  string
		call  func(*Client) error
		check func(error) bool
	}{
		{
			name: "get_available_models is readiness",
			kind: "get_available_models",
			call: func(c *Client) error {
				_, err := c.GetAvailableModels(context.Background())
				return err
			},
			check: func(err error) bool {
				var target *ReadinessError
				return errors.As(err, &target)
			},
		},
		{
			name: "set_model is readiness",
			kind: "set_model",
			call: func(c *Client) error {
				return c.SetModel(context.Background(), "acme", "m-1")
			},
			check: func(err error) bool {
				var target *ReadinessError
				return errors.As(err, &target)
			},
		},
		{
			name: "prompt is task failure",
			kind: "prompt",
			call: func(c *Client) error {
				return c.Prompt(context.Background(), "hello")
			},
			check: func(err error) bool {
				var target *TaskError
				return errors.As(err, &target)
			},
		},
		{
			name: "get_last_assistant_text is task failure",
			kind: "get_last_assistant_text",
			call: func(c *Client) error {
				_, err := c.GetLastAssistantText(context.Background())
				return err
			},
			check: func(err error) bool {
				var target *TaskError
				return errors.As(err, &target)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			script := &script.Script{Triggers: map[string][]script.Step{
				test.kind: {{Response: &script.Response{Success: false, Error: "boom"}}},
			}}
			proc := startScriptedPi(t, script)
			client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

			err := test.call(client)
			if err == nil {
				t.Fatalf("expected a failure")
			}
			if !test.check(err) {
				t.Fatalf("err = %T %v, want typed failure", err, err)
			}
			if !strings.Contains(err.Error(), "boom") {
				t.Fatalf("error = %q, want pi detail preserved", err)
			}
		})
	}
}

func TestClientSetModelRequiresExactConfirmation(t *testing.T) {
	// A success:true set_model response must carry a non-null object whose
	// provider and id strings exactly equal the requested catalog pair.
	// Missing, null, mistyped, and mismatched data are all protocol
	// violations, never silent successes.
	tests := []struct {
		name string
		data string
	}{
		{name: "missing data", data: ""},
		{name: "null data", data: "null"},
		{name: "data is array", data: `[]`},
		{name: "data is string", data: `"model"`},
		{name: "data is number", data: `7`},
		{name: "missing provider", data: `{"id":"m-1"}`},
		{name: "null provider", data: `{"provider":null,"id":"m-1"}`},
		{name: "provider is number", data: `{"provider":7,"id":"m-1"}`},
		{name: "provider is object", data: `{"provider":{},"id":"m-1"}`},
		{name: "missing id", data: `{"provider":"acme"}`},
		{name: "null id", data: `{"provider":"acme","id":null}`},
		{name: "id is number", data: `{"provider":"acme","id":7}`},
		{name: "mismatched provider", data: `{"provider":"other","id":"m-1"}`},
		{name: "mismatched id", data: `{"provider":"acme","id":"m-2"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			script := &script.Script{Triggers: map[string][]script.Step{
				"set_model": {{Response: &script.Response{Success: true, Data: json.RawMessage(test.data)}}},
			}}
			proc := startScriptedPi(t, script)
			client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

			err := client.SetModel(context.Background(), "acme", "m-1")
			var protocolErr *ProtocolError
			if !errors.As(err, &protocolErr) {
				t.Fatalf("err = %v, want *ProtocolError", err)
			}
		})
	}
}

func TestClientSetModelAcceptsExactConfirmation(t *testing.T) {
	// A success response whose data exactly confirms the requested
	// provider/id pair is accepted; extra catalog fields are opaque.
	script := &script.Script{Triggers: map[string][]script.Step{
		"set_model": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"provider":"acme","id":"m-1","label":"ignored"}`)}},
		},
	}}
	proc := startScriptedPi(t, script)
	client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

	if err := client.SetModel(context.Background(), "acme", "m-1"); err != nil {
		t.Fatalf("set model: %v", err)
	}
}

func TestClientGetAvailableThinkingLevelsAcceptsExactUniqueValues(t *testing.T) {
	script := &script.Script{Triggers: map[string][]script.Step{
		"get_available_thinking_levels": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"levels":["off","minimal","low","medium","high","xhigh","max"]}`)}},
		},
	}}
	proc := startScriptedPi(t, script)
	client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

	levels, err := client.GetAvailableThinkingLevels(context.Background())
	if err != nil {
		t.Fatalf("get available thinking levels: %v", err)
	}
	want := []ThinkingLevel{ThinkingOff, ThinkingMinimal, ThinkingLow, ThinkingMedium, ThinkingHigh, ThinkingXHigh, ThinkingMax}
	if !slices.Equal(levels, want) {
		t.Fatalf("levels = %v, want %v", levels, want)
	}
}

func TestClientGetAvailableThinkingLevelsRejectsInvalidSuccessData(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "missing data", data: ""},
		{name: "null data", data: "null"},
		{name: "missing levels", data: `{}`},
		{name: "null levels", data: `{"levels":null}`},
		{name: "data is array", data: `[]`},
		{name: "levels is object", data: `{"levels":{}}`},
		{name: "levels is string", data: `{"levels":"max"}`},
		{name: "unknown level", data: `{"levels":["ultra"]}`},
		{name: "duplicate level", data: `{"levels":["max","max"]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			script := &script.Script{Triggers: map[string][]script.Step{
				"get_available_thinking_levels": {{Response: &script.Response{Success: true, Data: json.RawMessage(test.data)}}},
			}}
			proc := startScriptedPi(t, script)
			client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

			_, err := client.GetAvailableThinkingLevels(context.Background())
			var protocolErr *ProtocolError
			if !errors.As(err, &protocolErr) {
				t.Fatalf("err = %v, want *ProtocolError", err)
			}
		})
	}
}

func TestClientSetThinkingLevelAcceptsSuccessAndTypesRejection(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		script := &script.Script{Triggers: map[string][]script.Step{
			"set_thinking_level": {{Response: &script.Response{Success: true}}},
		}}
		proc := startScriptedPi(t, script)
		client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

		if err := client.SetThinkingLevel(context.Background(), ThinkingMax); err != nil {
			t.Fatalf("set thinking level: %v", err)
		}
	})

	t.Run("rejection", func(t *testing.T) {
		script := &script.Script{Triggers: map[string][]script.Step{
			"set_thinking_level": {{Response: &script.Response{Success: false, Error: "SECRET-UPSTREAM-DETAIL"}}},
		}}
		proc := startScriptedPi(t, script)
		client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

		err := client.SetThinkingLevel(context.Background(), ThinkingMax)
		var rejected *ThinkingLevelRejectedError
		if !errors.As(err, &rejected) || rejected.Level != ThinkingMax {
			t.Fatalf("SetThinkingLevel error = %v, want max rejection", err)
		}
		if strings.Contains(err.Error(), "SECRET-UPSTREAM-DETAIL") {
			t.Fatalf("rejection leaked upstream detail: %q", err)
		}
	})
}

func TestClientGetStateAcceptsExactModelAndThinking(t *testing.T) {
	script := &script.Script{Triggers: map[string][]script.Step{
		"get_state": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"model":{"provider":"acme","id":"m-1","ignored":true},"thinkingLevel":"high","isStreaming":false}`)}},
		},
	}}
	proc := startScriptedPi(t, script)
	client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

	state, err := client.GetState(context.Background())
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	want := SessionState{Model: ModelProjection{Provider: "acme", ID: "m-1"}, ThinkingLevel: ThinkingHigh}
	if state != want {
		t.Fatalf("state = %#v, want %#v", state, want)
	}
}

func TestClientGetStateRejectsInvalidSuccessData(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "missing data", data: ""},
		{name: "null data", data: "null"},
		{name: "data is array", data: `[]`},
		{name: "missing model", data: `{"thinkingLevel":"high"}`},
		{name: "null model", data: `{"model":null,"thinkingLevel":"high"}`},
		{name: "missing provider", data: `{"model":{"id":"m-1"},"thinkingLevel":"high"}`},
		{name: "missing id", data: `{"model":{"provider":"acme"},"thinkingLevel":"high"}`},
		{name: "invalid selector", data: `{"model":{"provider":"acme","id":"m-1:max"},"thinkingLevel":"high"}`},
		{name: "missing thinking", data: `{"model":{"provider":"acme","id":"m-1"}}`},
		{name: "null thinking", data: `{"model":{"provider":"acme","id":"m-1"},"thinkingLevel":null}`},
		{name: "unknown thinking", data: `{"model":{"provider":"acme","id":"m-1"},"thinkingLevel":"ultra"}`},
		{name: "thinking is number", data: `{"model":{"provider":"acme","id":"m-1"},"thinkingLevel":7}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			script := &script.Script{Triggers: map[string][]script.Step{
				"get_state": {{Response: &script.Response{Success: true, Data: json.RawMessage(test.data)}}},
			}}
			proc := startScriptedPi(t, script)
			client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

			_, err := client.GetState(context.Background())
			var protocolErr *ProtocolError
			if !errors.As(err, &protocolErr) {
				t.Fatalf("err = %v, want *ProtocolError", err)
			}
		})
	}
}

func TestClientLastAssistantTextRejectsInvalidSuccessContainers(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "missing data", data: ""},
		{name: "null data", data: "null"},
		{name: "text is number", data: `{"text":7}`},
		{name: "data is array", data: `[]`},
		{name: "data is string", data: `"text"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			script := &script.Script{Triggers: map[string][]script.Step{
				"get_last_assistant_text": {{Response: &script.Response{Success: true, Data: json.RawMessage(test.data)}}},
			}}
			proc := startScriptedPi(t, script)
			client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

			_, err := client.GetLastAssistantText(context.Background())
			var protocolErr *ProtocolError
			if !errors.As(err, &protocolErr) {
				t.Fatalf("err = %v, want *ProtocolError", err)
			}
		})
	}
}

func TestClientLastAssistantTextNull(t *testing.T) {
	script := &script.Script{Triggers: map[string][]script.Step{
		"get_last_assistant_text": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"text":null}`)}},
		},
	}}
	proc := startScriptedPi(t, script)
	client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

	text, err := client.GetLastAssistantText(context.Background())
	if err != nil {
		t.Fatalf("get last assistant text: %v", err)
	}
	if text != "" {
		t.Fatalf("text = %q, want empty", text)
	}
}

func TestClientLastAssistantTextMissingTextFieldIsEmpty(t *testing.T) {
	// Observed Pi 0.84.1 behavior: the server serializes the undefined
	// "no assistant text" value as an omitted text key (data:{}), never as
	// text:null. The missing field means empty text, not a protocol
	// violation.
	script := &script.Script{Triggers: map[string][]script.Step{
		"get_last_assistant_text": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{}`)}},
		},
	}}
	proc := startScriptedPi(t, script)
	client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

	text, err := client.GetLastAssistantText(context.Background())
	if err != nil {
		t.Fatalf("get last assistant text: %v", err)
	}
	if text != "" {
		t.Fatalf("text = %q, want empty", text)
	}
}

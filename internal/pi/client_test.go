package pi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/piversion"
	"github.com/arasovic/pi-worker/internal/testutil/fakepi/script"
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

type pumpBlockingReader struct {
	started   chan struct{}
	unblock   chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
}

func newPumpBlockingReader() *pumpBlockingReader {
	return &pumpBlockingReader{
		started: make(chan struct{}),
		unblock: make(chan struct{}),
	}
}

func (r *pumpBlockingReader) Read(_ []byte) (int, error) {
	r.startOnce.Do(func() {
		close(r.started)
	})
	<-r.unblock
	return 0, io.ErrClosedPipe
}

func (r *pumpBlockingReader) Close() error {
	r.closeOnce.Do(func() {
		close(r.unblock)
	})
	return nil
}

func TestClientCloseStopsFramePump(t *testing.T) {
	reader := newPumpBlockingReader()
	client := NewClient(io.Discard, reader, nil, nil)

	select {
	case <-reader.started:
	case <-time.After(time.Second):
		t.Fatalf("frame pump did not start within 1s")
	}

	done := make(chan error, 1)
	go func() {
		done <- client.Close()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("first client.Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("first client.Close did not return within 1s")
	}

	if err := client.Close(); err != nil {
		t.Fatalf("second client.Close: %v", err)
	}
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

func TestClientAcceptsLargeLegitimateEventAndFinalTextFrames(t *testing.T) {
	large := strings.Repeat("x", (1<<20)+1)
	event, err := json.Marshal(map[string]any{
		"type":       "tool_execution_end",
		"toolCallId": "large-read",
		"toolName":   "read",
		"result":     large,
	})
	if err != nil {
		t.Fatalf("marshal large event: %v", err)
	}
	finalData, err := json.Marshal(map[string]string{"text": large})
	if err != nil {
		t.Fatalf("marshal large final text: %v", err)
	}

	scriptConfig := &script.Script{Triggers: map[string][]script.Step{
		"prompt": {
			{Response: &script.Response{Success: true}},
			{Event: event},
			{Event: json.RawMessage(`{"type":"agent_settled"}`)},
		},
		"get_last_assistant_text": {
			{Response: &script.Response{Success: true, Data: finalData}},
		},
	}}
	proc := startScriptedPi(t, scriptConfig)
	client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

	if err := client.Prompt(context.Background(), "hello"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if err := client.WaitSettled(context.Background()); err != nil {
		t.Fatalf("wait settled: %v", err)
	}
	text, err := client.GetLastAssistantText(context.Background())
	if err != nil {
		t.Fatalf("get large final text: %v", err)
	}
	if text != large {
		t.Fatalf("final text length = %d, want %d", len(text), len(large))
	}
}

func TestClientWaitSettledRejectsUnsolicitedResponse(t *testing.T) {
	scriptConfig := &script.Script{Triggers: map[string][]script.Step{
		"prompt": {
			{Response: &script.Response{Success: true}},
			{Response: &script.Response{Success: true}},
		},
	}}
	proc := startScriptedPi(t, scriptConfig)
	client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

	if err := client.Prompt(context.Background(), "hello"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if err := client.WaitSettled(context.Background()); err == nil || err.Error() != "protocol error: unexpected response while waiting for agent_settled" {
		t.Fatalf("WaitSettled = %v, want exact unsolicited-response protocol error", err)
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

func TestClientMatchingResponseWithoutSuccessFailsExactProtocolError(t *testing.T) {
	scriptConfig := &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {{Raw: `{"type":"response","id":"r1","command":"get_available_models"}`}},
	}}
	proc := startScriptedPi(t, scriptConfig)
	client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

	_, err := client.GetAvailableModels(context.Background())
	if err == nil || err.Error() != "protocol error: response missing success field" {
		t.Fatalf("GetAvailableModels = %v, want exact missing-success protocol error", err)
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

func TestClientFrameWithoutTypeFails(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"success":true}`} {
		t.Run(raw, func(t *testing.T) {
			script := &script.Script{Triggers: map[string][]script.Step{
				"get_available_models": {
					{Raw: raw},
					{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[]}`)}},
				},
			}}
			proc := startScriptedPi(t, script)
			client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

			_, err := client.GetAvailableModels(context.Background())
			var protocolErr *ProtocolError
			if !errors.As(err, &protocolErr) {
				t.Fatalf("err = %v, want *ProtocolError", err)
			}
			if !strings.Contains(err.Error(), "missing type") {
				t.Fatalf("error = %q", err)
			}
		})
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
	for _, want := range []string{piversion.VerifiedVersion, "compatib", "provider", "login"} {
		if !strings.Contains(lower, strings.ToLower(want)) {
			t.Fatalf("error = %q, want detail %q", message, want)
		}
	}
	if strings.Contains(message, "FAKEPI-STDERR-SECRET-4e3c") {
		t.Fatalf("error exposed child stderr: %q", message)
	}
}

func TestClientStartupWriteFailureIsReadinessError(t *testing.T) {
	client := NewClient(closedPipeWriter{}, io.NopCloser(strings.NewReader("")), nil, nil)

	_, err := client.GetAvailableModels(context.Background())
	var readinessErr *ReadinessError
	if !errors.As(err, &readinessErr) {
		t.Fatalf("err = %v, want *ReadinessError", err)
	}
	message := err.Error()
	lower := strings.ToLower(message)
	for _, want := range []string{piversion.VerifiedVersion, "compatib", "provider", "login"} {
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

// The catalog read is a faithful transport: an entry whose provider itself
// contains a slash can never be named by any selector — the provider is
// whatever precedes the first slash — and is carried through, not grounds
// for discarding the whole catalog. Deciding what to publish belongs to the
// command that lists models, which is the only place that can also say how
// many were left out.
func TestClientGetAvailableModelsPassesUnnameableEntriesThrough(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		id       string
	}{
		{name: "slash in provider", provider: "ac/me", id: "model"},
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
			models, err := NewClient(proc.Stdin(), proc.Stdout(), nil, nil).GetAvailableModels(context.Background())
			if err != nil {
				t.Fatalf("GetAvailableModels = %v, want the entry carried through", err)
			}
			if len(models) != 1 || models[0].Provider != test.provider || models[0].ID != test.id {
				t.Fatalf("models = %v, want the entry unchanged", models)
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

func TestClientGetAvailableModelsAcceptsColonAndSpaceIds(t *testing.T) {
	// A routing-provider catalog reports ids such as "model:free" next to
	// "model". Those are real entries, so the transport must carry them:
	// the colon and the whitespace are id contents, and only the catalog
	// decides whether a name is usable.
	tests := []struct {
		name     string
		provider string
		id       string
	}{
		{name: "colon in id", provider: "acme", id: "model:free"},
		{name: "space in id", provider: "acme", id: "mo del"},
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
			models, err := NewClient(proc.Stdin(), proc.Stdout(), nil, nil).GetAvailableModels(context.Background())
			if err != nil {
				t.Fatalf("GetAvailableModels = %v, want the entry accepted", err)
			}
			if len(models) != 1 || models[0].Provider != test.provider || models[0].ID != test.id {
				t.Fatalf("models = %v, want the entry unchanged", models)
			}
		})
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
		{name: "provider with slash", data: `{"model":{"provider":"ac/me","id":"m-1"},"thinkingLevel":"high"}`},
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

func TestClientSteerSendsTypedRequestAndReportsSuccess(t *testing.T) {
	scriptConfig := &script.Script{Triggers: map[string][]script.Step{
		"steer": {{Response: &script.Response{Success: true}}},
	}}
	proc := startScriptedPi(t, scriptConfig)
	client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

	if err := client.Steer(context.Background(), "change direction"); err != nil {
		t.Fatalf("steer: %v", err)
	}
}

func TestClientSteerRejectsCorrelatedFailureAsTypedTaskError(t *testing.T) {
	scriptConfig := &script.Script{Triggers: map[string][]script.Step{
		"steer": {{Response: &script.Response{Success: false, Error: "boom"}}},
	}}
	proc := startScriptedPi(t, scriptConfig)
	client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

	err := client.Steer(context.Background(), "change direction")
	if err == nil {
		t.Fatalf("Steer returned nil, want typed failure")
	}
	var taskErr *TaskError
	if !errors.As(err, &taskErr) {
		t.Fatalf("err = %T %v, want *TaskError", err, err)
	}
	if !strings.HasPrefix(taskErr.Message, "steer: ") {
		t.Fatalf("TaskError.Message = %q, want stable \"steer: \" prefix", taskErr.Message)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %q, want pi detail preserved", err)
	}
}

func TestClientSteerRejectsEmptyMessageBeforeWrite(t *testing.T) {
	scriptConfig := &script.Script{Triggers: map[string][]script.Step{
		"steer": {{Response: &script.Response{Success: true}}},
	}}
	logPath := setupFakePiEnv(t, scriptConfig)
	proc, err := NewProcess(fakePiBin, t.TempDir())
	if err != nil {
		t.Fatalf("new process: %v", err)
	}
	if err := proc.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = proc.Close() })
	client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

	err = client.Steer(context.Background(), "")
	var taskErr *TaskError
	if !errors.As(err, &taskErr) {
		t.Fatalf("err = %T %v, want *TaskError", err, err)
	}
	if !strings.HasPrefix(taskErr.Message, "steer message must be non-empty") {
		t.Fatalf("TaskError.Message = %q, want stable empty-message prefix", taskErr.Message)
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(readRequestLog(logPath)) > 0 {
			t.Fatalf("steer wrote a frame despite empty message; log = %v", readRequestLog(logPath))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestClientSteerContextDoneBeforeWriteReturnsContextError(t *testing.T) {
	scriptConfig := &script.Script{Triggers: map[string][]script.Step{
		"steer": {{Response: &script.Response{Success: true}}},
	}}
	logPath := setupFakePiEnv(t, scriptConfig)
	proc, err := NewProcess(fakePiBin, t.TempDir())
	if err != nil {
		t.Fatalf("new process: %v", err)
	}
	if err := proc.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = proc.Close() })
	client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = client.Steer(ctx, "change direction")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Steer err = %v, want context.Canceled", err)
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(readRequestLog(logPath)) > 0 {
			t.Fatalf("Steer wrote a frame after context cancellation; log = %v", readRequestLog(logPath))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestClientAbortSendsTypedRequestAndReportsSuccess(t *testing.T) {
	scriptConfig := &script.Script{Triggers: map[string][]script.Step{
		"abort": {{Response: &script.Response{Success: true}}},
	}}
	proc := startScriptedPi(t, scriptConfig)
	client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

	if err := client.Abort(context.Background()); err != nil {
		t.Fatalf("abort: %v", err)
	}
}

func TestClientAbortRejectsCorrelatedFailureAsTypedTaskError(t *testing.T) {
	scriptConfig := &script.Script{Triggers: map[string][]script.Step{
		"abort": {{Response: &script.Response{Success: false, Error: "boom"}}},
	}}
	proc := startScriptedPi(t, scriptConfig)
	client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

	err := client.Abort(context.Background())
	if err == nil {
		t.Fatalf("Abort returned nil, want typed failure")
	}
	var taskErr *TaskError
	if !errors.As(err, &taskErr) {
		t.Fatalf("err = %T %v, want *TaskError", err, err)
	}
	if !strings.HasPrefix(taskErr.Message, "abort: ") {
		t.Fatalf("TaskError.Message = %q, want stable \"abort: \" prefix", taskErr.Message)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %q, want pi detail preserved", err)
	}
}

func TestClientAbortContextDoneBeforeWriteReturnsContextError(t *testing.T) {
	scriptConfig := &script.Script{Triggers: map[string][]script.Step{
		"abort": {{Response: &script.Response{Success: true}}},
	}}
	logPath := setupFakePiEnv(t, scriptConfig)
	proc, err := NewProcess(fakePiBin, t.TempDir())
	if err != nil {
		t.Fatalf("new process: %v", err)
	}
	if err := proc.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = proc.Close() })
	client := NewClient(proc.Stdin(), proc.Stdout(), nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = client.Abort(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Abort err = %v, want context.Canceled", err)
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(readRequestLog(logPath)) > 0 {
			t.Fatalf("Abort wrote a frame after context cancellation; log = %v", readRequestLog(logPath))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// newControlledPeer wires a full-duplex net.Pipe to one Client and one
// FrameReader/FrameWriter pair so a test can drive the documented JSONL
// RPC stream against WaitSettledControlled without fakepi. serverConn is
// the side the test reads requests from and writes responses through;
// clientConn is what the Client reads from and writes to. Cleanup closes
// the Client, then both connection halves, so a test never repeats the
// teardown or leaks a blocked frame-pump goroutine.
func newControlledPeer(t *testing.T, handler EventHandler) (*Client, *FrameReader, *FrameWriter, net.Conn) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	client := NewClient(clientConn, clientConn, handler, nil)
	reader := NewFrameReader(serverConn, MaxFrameBytes)
	writer := NewFrameWriter(serverConn)
	t.Cleanup(func() {
		_ = client.Close()
		_ = serverConn.Close()
		_ = clientConn.Close()
	})
	return client, reader, writer, serverConn
}

// readControlRequest reads one outbound request from the test's side of the
// pipe and fails the test if it cannot be parsed or if it is not the typed
// control the test expects. kind selects the wire command; wantMessage is
// compared against the request's message field when non-empty (steer) and
// against the empty string when wantMessage is empty (abort). It returns
// the parsed request so the test can correlate its id with the response.
func readControlRequest(t *testing.T, reader *FrameReader, kind WorkerControlKind, wantMessage string) request {
	t.Helper()
	frame, err := reader.ReadFrame()
	if err != nil {
		t.Fatalf("read control request: %v", err)
	}
	var req request
	if err := json.Unmarshal(frame, &req); err != nil {
		t.Fatalf("unmarshal control request: %v", err)
	}
	if req.Type != string(kind) {
		t.Fatalf("control request type = %q, want %q", req.Type, kind)
	}
	if wantMessage != "" {
		if req.Message != wantMessage {
			t.Fatalf("control message = %q, want %q", req.Message, wantMessage)
		}
	} else if req.Message != "" {
		t.Fatalf("control carried a message %q, want none", req.Message)
	}
	if req.ID == "" {
		t.Fatalf("control request has empty id")
	}
	return req
}

// writeSuccessResponse emits one correlated success response to the
// FrameWriter so the client's pending control resolves without a frame
// write failing the test.
func writeSuccessResponse(t *testing.T, writer *FrameWriter, id, command string) {
	t.Helper()
	if err := writer.WriteFrame(map[string]any{
		"type":    "response",
		"id":      id,
		"command": command,
		"success": true,
	}); err != nil {
		t.Fatalf("write %s response: %v", command, err)
	}
}

// writeMismatchCommandResponse emits one correlated response whose command
// does not match the request command, the documented protocol violation
// that ends WaitSettledControlled with a *ProtocolError.
func writeMismatchCommandResponse(t *testing.T, writer *FrameWriter, id, command string) {
	t.Helper()
	if err := writer.WriteFrame(map[string]any{
		"type":    "response",
		"id":      id,
		"command": command,
		"success": true,
	}); err != nil {
		t.Fatalf("write mismatched-command response: %v", err)
	}
}

// writeAgentSettled writes the terminal agent_settled event so the
// controlled wait can complete without spinning on the frame channel.
func writeAgentSettled(t *testing.T, writer *FrameWriter) {
	t.Helper()
	if err := writer.WriteFrame(json.RawMessage(`{"type":"agent_settled"}`)); err != nil {
		t.Fatalf("write agent_settled: %v", err)
	}
}

// runPromptRoundTrip drives Prompt on the Client so the test peer can
// observe and respond to the prompt request before WaitSettledControlled
// is invoked. The prompt round-trip must complete before the controlled
// wait starts, because Prompt sets awaitingSettled = true only after the
// success response has been consumed.
func runPromptRoundTrip(t *testing.T, client *Client, reader *FrameReader, writer *FrameWriter, message string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	promptDone := make(chan error, 1)
	go func() {
		promptDone <- client.Prompt(ctx, message)
	}()

	frame, err := reader.ReadFrame()
	if err != nil {
		t.Fatalf("read prompt request: %v", err)
	}
	var promptReq request
	if err := json.Unmarshal(frame, &promptReq); err != nil {
		t.Fatalf("unmarshal prompt request: %v", err)
	}
	if promptReq.Type != requestPrompt {
		t.Fatalf("prompt request type = %q, want %q", promptReq.Type, requestPrompt)
	}
	if promptReq.Message != message {
		t.Fatalf("prompt message = %q, want %q", promptReq.Message, message)
	}
	writeSuccessResponse(t, writer, promptReq.ID, requestPrompt)

	select {
	case err := <-promptDone:
		if err != nil {
			t.Fatalf("prompt: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("prompt did not complete within 1s")
	}
}

// TestClientWaitSettledControlledRoutesSteerAndAbort drives one steer and
// one abort control through WaitSettledControlled. The peer reads the
// prompt request, completes the prompt round-trip, reads the typed
// control request, interleaves one harmless event before the success
// response, then sends agent_settled. Both the buffered control Result
// and the wait must return nil, and the recording handler must observe
// the harmless event and agent_settled in wire order.
func TestClientWaitSettledControlledRoutesSteerAndAbort(t *testing.T) {
	tests := []struct {
		name        string
		kind        WorkerControlKind
		message     string
		wantMessage string
	}{
		{name: "steer carries message", kind: WorkerControlSteer, message: "change direction", wantMessage: "change direction"},
		{name: "abort has no message", kind: WorkerControlAbort, message: "", wantMessage: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := &recordingHandler{}
			client, reader, writer, _ := newControlledPeer(t, handler)
			runPromptRoundTrip(t, client, reader, writer, "hello")

			controls := make(chan WorkerControl, 1)
			ctrlResult := make(chan error, 1)
			controls <- WorkerControl{Kind: tc.kind, Message: tc.message, Result: ctrlResult}

			waitDone := make(chan error, 1)
			go func() {
				waitDone <- client.WaitSettledControlled(context.Background(), controls)
			}()

			req := readControlRequest(t, reader, tc.kind, tc.wantMessage)

			if err := writer.WriteFrame(json.RawMessage(`{"type":"message_update"}`)); err != nil {
				t.Fatalf("write harmless event: %v", err)
			}
			writeSuccessResponse(t, writer, req.ID, string(tc.kind))
			writeAgentSettled(t, writer)

			select {
			case err := <-ctrlResult:
				if err != nil {
					t.Fatalf("control result = %v, want nil", err)
				}
			case <-time.After(time.Second):
				t.Fatalf("control result did not arrive within 1s")
			}

			select {
			case err := <-waitDone:
				if err != nil {
					t.Fatalf("WaitSettledControlled = %v, want nil", err)
				}
			case <-time.After(time.Second):
				t.Fatalf("WaitSettledControlled did not return within 1s")
			}

			wantEvents := []string{"message_update", "agent_settled"}
			if !slices.Equal(handler.types(), wantEvents) {
				t.Fatalf("handler events = %v, want %v", handler.types(), wantEvents)
			}
		})
	}
}

// TestClientWaitSettledControlledConsumesSettlementBeforeControlResponse
// proves the settlement-before-control-response ordering: agent_settled
// must be retained until the in-flight control's correlated response has
// been consumed, so the wait does not return early when settlement races
// ahead of a pending control. After the agent_settled event has been
// delivered, a bounded non-return assertion proves the wait is still in
// flight; the success response then clears the pending state, the wait
// returns nil, and both the buffered control Result and the wait observe
// the same nil outcome.
func TestClientWaitSettledControlledConsumesSettlementBeforeControlResponse(t *testing.T) {
	client, reader, writer, _ := newControlledPeer(t, &recordingHandler{})
	runPromptRoundTrip(t, client, reader, writer, "hello")

	controls := make(chan WorkerControl, 1)
	ctrlResult := make(chan error, 1)
	controls <- WorkerControl{Kind: WorkerControlSteer, Message: "change direction", Result: ctrlResult}

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- client.WaitSettledControlled(context.Background(), controls)
	}()

	req := readControlRequest(t, reader, WorkerControlSteer, "change direction")

	writeAgentSettled(t, writer)

	select {
	case err := <-waitDone:
		t.Fatalf("WaitSettledControlled returned before the control response: err = %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	writeSuccessResponse(t, writer, req.ID, requestSteer)

	select {
	case err := <-ctrlResult:
		if err != nil {
			t.Fatalf("control result = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("control result did not arrive within 1s")
	}

	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("WaitSettledControlled = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("WaitSettledControlled did not return within 1s")
	}
}

// TestClientWaitSettledControlledRejectsPreWriteControls proves that
// controls rejected before any frame is written — an empty steer message
// and an unknown WorkerControlKind — surface as a typed *TaskError on
// the buffered control Result without writing a frame to the peer. The
// peer must then be able to drive the wait to completion with the
// terminal agent_settled event.
func TestClientWaitSettledControlledRejectsPreWriteControls(t *testing.T) {
	tests := []struct {
		name    string
		kind    WorkerControlKind
		message string
		wantMsg string
	}{
		{name: "empty steer", kind: WorkerControlSteer, message: "", wantMsg: "steer message must be non-empty"},
		{name: "unknown kind", kind: WorkerControlKind("interrupt"), message: "", wantMsg: `unknown worker control kind "interrupt"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, reader, writer, serverConn := newControlledPeer(t, &recordingHandler{})
			runPromptRoundTrip(t, client, reader, writer, "hello")

			controls := make(chan WorkerControl, 1)
			ctrlResult := make(chan error, 1)
			controls <- WorkerControl{Kind: tc.kind, Message: tc.message, Result: ctrlResult}

			waitDone := make(chan error, 1)
			go func() {
				waitDone <- client.WaitSettledControlled(context.Background(), controls)
			}()

			var ctrlErr error
			select {
			case ctrlErr = <-ctrlResult:
			case <-time.After(time.Second):
				t.Fatalf("control result did not arrive within 1s")
			}
			if ctrlErr == nil {
				t.Fatalf("control result = nil, want a typed error")
			}
			var taskErr *TaskError
			if !errors.As(ctrlErr, &taskErr) {
				t.Fatalf("control result = %T %v, want *TaskError", ctrlErr, ctrlErr)
			}
			if taskErr.Message != tc.wantMsg {
				t.Fatalf("TaskError.Message = %q, want %q", taskErr.Message, tc.wantMsg)
			}

			if err := serverConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
				t.Fatalf("set read deadline: %v", err)
			}
			if frame, err := reader.ReadFrame(); err == nil {
				t.Fatalf("peer received an unexpected frame despite pre-write rejection: %s", frame)
			} else if !isReadTimeout(err) {
				t.Fatalf("peer read after rejection: %v", err)
			}

			writeAgentSettled(t, writer)

			select {
			case err := <-waitDone:
				if err != nil {
					t.Fatalf("WaitSettledControlled = %v, want nil", err)
				}
			case <-time.After(time.Second):
				t.Fatalf("WaitSettledControlled did not return within 1s")
			}
		})
	}
}

// isReadTimeout reports whether the FrameReader read timed out without
// any data arriving, used to assert that no control frame was written.
func isReadTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// TestClientWaitSettledControlledFailuresPropagateToControlResult proves
// that control-protocol and control-transport failures end
// WaitSettledControlled with an error that is also delivered to the
// in-flight control's buffered Result: a correlated response whose
// command does not match the request command must surface as the same
// *ProtocolError on both, and closing the peer while a control is
// pending must surface as the same *transportError on both.
func TestClientWaitSettledControlledFailuresPropagateToControlResult(t *testing.T) {
	t.Run("mismatched response command", func(t *testing.T) {
		client, reader, writer, _ := newControlledPeer(t, nil)
		runPromptRoundTrip(t, client, reader, writer, "hello")

		controls := make(chan WorkerControl, 1)
		ctrlResult := make(chan error, 1)
		controls <- WorkerControl{Kind: WorkerControlSteer, Message: "change direction", Result: ctrlResult}

		waitDone := make(chan error, 1)
		go func() {
			waitDone <- client.WaitSettledControlled(context.Background(), controls)
		}()

		req := readControlRequest(t, reader, WorkerControlSteer, "change direction")
		writeMismatchCommandResponse(t, writer, req.ID, requestAbort)

		var ctrlErr error
		select {
		case ctrlErr = <-ctrlResult:
		case <-time.After(time.Second):
			t.Fatalf("control result did not arrive within 1s")
		}
		var waitErr error
		select {
		case waitErr = <-waitDone:
		case <-time.After(time.Second):
			t.Fatalf("WaitSettledControlled did not return within 1s")
		}

		var ctrlProtocolErr *ProtocolError
		if !errors.As(ctrlErr, &ctrlProtocolErr) {
			t.Fatalf("control result = %T %v, want *ProtocolError", ctrlErr, ctrlErr)
		}
		var waitProtocolErr *ProtocolError
		if !errors.As(waitErr, &waitProtocolErr) {
			t.Fatalf("WaitSettledControlled = %T %v, want *ProtocolError", waitErr, waitErr)
		}
		if ctrlProtocolErr.Error() != waitProtocolErr.Error() {
			t.Fatalf("control result = %q, want same protocol error as wait %q", ctrlProtocolErr.Error(), waitProtocolErr.Error())
		}
		if !strings.Contains(waitProtocolErr.Error(), "does not match") {
			t.Fatalf("WaitSettledControlled error = %q, want command-mismatch detail", waitProtocolErr.Error())
		}
	})

	t.Run("server closes while control pending", func(t *testing.T) {
		client, reader, writer, serverConn := newControlledPeer(t, nil)
		runPromptRoundTrip(t, client, reader, writer, "hello")

		controls := make(chan WorkerControl, 1)
		ctrlResult := make(chan error, 1)
		controls <- WorkerControl{Kind: WorkerControlSteer, Message: "change direction", Result: ctrlResult}

		waitDone := make(chan error, 1)
		go func() {
			waitDone <- client.WaitSettledControlled(context.Background(), controls)
		}()

		readControlRequest(t, reader, WorkerControlSteer, "change direction")
		if err := serverConn.Close(); err != nil {
			t.Fatalf("close server connection: %v", err)
		}

		var ctrlErr error
		select {
		case ctrlErr = <-ctrlResult:
		case <-time.After(time.Second):
			t.Fatalf("control result did not arrive within 1s")
		}
		var waitErr error
		select {
		case waitErr = <-waitDone:
		case <-time.After(time.Second):
			t.Fatalf("WaitSettledControlled did not return within 1s")
		}

		var ctrlTransportErr *transportError
		if !errors.As(ctrlErr, &ctrlTransportErr) {
			t.Fatalf("control result = %T %v, want *transportError", ctrlErr, ctrlErr)
		}
		var waitTransportErr *transportError
		if !errors.As(waitErr, &waitTransportErr) {
			t.Fatalf("WaitSettledControlled = %T %v, want *transportError", waitErr, waitErr)
		}
		if ctrlTransportErr.Error() != waitTransportErr.Error() {
			t.Fatalf("control result = %q, want same transport error as wait %q", ctrlTransportErr.Error(), waitTransportErr.Error())
		}
	})
}

// TestClientWaitSettledClosesReaderReturnsTransportError proves that
// Close drives a transport-error exit on a blocking WaitSettled.
func TestClientWaitSettledClosesReaderReturnsTransportError(t *testing.T) {
	reader := newPumpBlockingReader()
	client := NewClient(io.Discard, reader, nil, nil)

	select {
	case <-reader.started:
	case <-time.After(time.Second):
		t.Fatalf("frame pump did not start within 1s")
	}

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- client.WaitSettled(context.Background())
	}()

	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case err := <-waitDone:
		var transportErr *transportError
		if !errors.As(err, &transportErr) {
			t.Fatalf("WaitSettled returned %T %v, want *transportError", err, err)
		}
	case <-time.After(time.Second):
		t.Fatalf("WaitSettled did not return within 1s after Close")
	}
}

// TestClientWaitSettledControlledClosesReaderReturnsTransportError
// proves that Close drives a transport-error exit on a blocking
// WaitSettledControlled when no controls are ever sent (nil channel).
func TestClientWaitSettledControlledClosesReaderReturnsTransportError(t *testing.T) {
	reader := newPumpBlockingReader()
	client := NewClient(io.Discard, reader, nil, nil)

	select {
	case <-reader.started:
	case <-time.After(time.Second):
		t.Fatalf("frame pump did not start within 1s")
	}

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- client.WaitSettledControlled(context.Background(), nil)
	}()

	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case err := <-waitDone:
		var transportErr *transportError
		if !errors.As(err, &transportErr) {
			t.Fatalf("WaitSettledControlled returned %T %v, want *transportError", err, err)
		}
	case <-time.After(time.Second):
		t.Fatalf("WaitSettledControlled did not return within 1s after Close")
	}
}

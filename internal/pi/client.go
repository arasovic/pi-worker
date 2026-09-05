package pi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arasovic/pi-worker/internal/piversion"
)

const eventAgentSettled = "agent_settled"

// EventHandler receives every non-response frame. A handler error is treated
// as a protocol violation and fails the client deterministically.
type EventHandler interface {
	OnEvent(Event) error
}

// eventHandlers fans one event stream out to several EventHandlers in
// order, returning the first error. It is itself an EventHandler, so a
// client with a single handler slot can let two accumulators observe the
// same stream through one value.
type eventHandlers []EventHandler

func (h eventHandlers) OnEvent(event Event) error {
	for _, handler := range h {
		if err := handler.OnEvent(event); err != nil {
			return err
		}
	}
	return nil
}

// debugHeartbeatInterval is the visible-line silence interval for the
// worker-scoped lifecycle heartbeat.
const debugHeartbeatInterval = 30 * time.Second

// toolStartCap bounds the tool-execution start timings the client retains
// for completion-duration correlation. Tool call ids are Pi-controlled
// untrusted input, so an unmatched flood of unique ids must not grow memory
// without bound. The cap is one eighth of the debug line budget, which
// already bounds the debug lines such starts can emit. Once the cap is
// reached, new starts still emit their safe started line but are not
// tracked, so their completions report no duration instead of a false one.
const toolStartCap = debugLineBudget / 8

// toolCallIDMaxBytes bounds one retained tool-call id in bytes. Pi controls
// the id and one frame may approach the multi-megabyte record limit, so an id is
// only eligible for timing correlation when it fits this small fixed bound:
// oversized and empty ids are never retained, their safe start/end lines
// still appear, and their completions omit the duration. The id is only
// ever used as a map key, never logged.
const toolCallIDMaxBytes = 128

// Client drives the seven documented outbound RPC request types over a Pi
// JSONL stream. Requests carry generated IDs; responses are correlated by ID
// while events interleave. The client is single-flight: one request at a
// time, matching the worker's linear prompt lifecycle. There is no arbitrary
// caller JSON API and no direct RPC bash surface.
type frameReadResult struct {
	frame []byte
	err   error
}

type Client struct {
	in      *FrameReader
	out     *FrameWriter
	handler EventHandler
	nextID  int
	settled bool
	// awaitingSettled gates which settlement event is terminal: only events
	// from the prompt lifecycle become terminal and log "agent settled".
	awaitingSettled bool
	debug           *WorkerScope
	// modelPhase is the last projected assistant message phase. Only the
	// single driving goroutine touches it.
	modelPhase string
	// toolStarts correlates tool_execution_start/end timings by the
	// internal tool-call identifier, bounded by toolStartCap. The
	// identifier is Pi-controlled untrusted input and is only ever used
	// as this map key, never logged. Only the single driving goroutine
	// touches it.
	toolStarts map[string]time.Duration

	frameResults chan frameReadResult
	stop         chan struct{}
	done         chan struct{}
	closer       io.Closer
	closeOnce    sync.Once
	closeErr     error
}

func NewClient(stdin io.Writer, stdout io.Reader, handler EventHandler, debug *WorkerScope) *Client {
	client := &Client{
		in:           NewFrameReader(stdout, MaxFrameBytes),
		out:          NewFrameWriter(stdin),
		handler:      handler,
		debug:        debug,
		frameResults: make(chan frameReadResult, 1),
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
	}
	if closer, ok := stdout.(io.Closer); ok {
		client.closer = closer
	}
	go client.pumpFrames()
	return client
}

func (c *Client) nextRequestID() string {
	c.nextID++
	return fmt.Sprintf("r%d", c.nextID)
}

// GetAvailableModels requests the current available-model catalog.
func (c *Client) GetAvailableModels(ctx context.Context) ([]ModelProjection, error) {
	req, err := newRequest(requestGetAvailableModels)
	if err != nil {
		return nil, err
	}
	resp, err := c.roundTrip(ctx, req)
	if err != nil {
		var transportErr *transportError
		if errors.As(err, &transportErr) {
			return nil, &ReadinessError{Message: fmt.Sprintf("pi exited before returning the model catalog; verify Pi %s compatibility and provider login", piversion.VerifiedVersion)}
		}
		return nil, err
	}
	if !*resp.Success {
		return nil, &ReadinessError{Message: "get_available_models: " + responseDetail(resp)}
	}
	if len(resp.Data) == 0 || isJSONNull(resp.Data) {
		return nil, newProtocolError("get_available_models data must be an object with a models array")
	}
	var data struct {
		Models []ModelProjection `json:"models"`
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return nil, newProtocolError("malformed get_available_models data: %v", err)
	}
	if data.Models == nil {
		return nil, newProtocolError("get_available_models data missing models array")
	}
	// An entry this build cannot name is reported and left out by the caller,
	// never grounds for discarding the catalog: one unusable entry among many
	// would otherwise hide every usable one.
	return data.Models, nil
}

// SetModel activates the exact catalog provider/id. Pi confirms a
// successful set_model with data:Model, so v0 requires that confirmation to
// be a non-null object whose provider and id strings exactly equal the
// requested catalog pair. A missing, null, mistyped, or mismatched
// confirmation is a protocol violation, never a silent success.
func (c *Client) SetModel(ctx context.Context, provider, modelID string) error {
	req, err := newRequest(requestSetModel)
	if err != nil {
		return err
	}
	req.Provider = provider
	req.ModelID = modelID
	resp, err := c.roundTrip(ctx, req)
	if err != nil {
		return err
	}
	if !*resp.Success {
		return &ReadinessError{Message: "set_model: " + responseDetail(resp)}
	}
	if len(resp.Data) == 0 || isJSONNull(resp.Data) {
		return newProtocolError("set_model data must be an object with provider and id")
	}
	var data ModelProjection
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return newProtocolError("malformed set_model data: %v", err)
	}
	if data.Provider != provider || data.ID != modelID {
		return newProtocolError("set_model confirmed %q/%q, expected %q/%q", data.Provider, data.ID, provider, modelID)
	}
	return nil
}

// GetAvailableThinkingLevels returns the exact unique thinking levels Pi
// reports for the active model. Unknown or duplicate values are protocol
// violations: pi-worker accepts only the vocabulary of the Pi version
// recorded in the pin.
func (c *Client) GetAvailableThinkingLevels(ctx context.Context) ([]ThinkingLevel, error) {
	req, err := newRequest(requestGetAvailableThinkingLevels)
	if err != nil {
		return nil, err
	}
	resp, err := c.roundTrip(ctx, req)
	if err != nil {
		return nil, err
	}
	if !*resp.Success {
		return nil, &ReadinessError{Message: "get_available_thinking_levels: " + responseDetail(resp)}
	}
	if len(resp.Data) == 0 || isJSONNull(resp.Data) {
		return nil, newProtocolError("get_available_thinking_levels data must be an object with a levels array")
	}
	var container struct {
		Levels json.RawMessage `json:"levels"`
	}
	if err := json.Unmarshal(resp.Data, &container); err != nil {
		return nil, newProtocolError("malformed get_available_thinking_levels data: %v", err)
	}
	if len(container.Levels) == 0 || isJSONNull(container.Levels) {
		return nil, newProtocolError("get_available_thinking_levels data missing levels array")
	}
	var rawLevels []json.RawMessage
	if err := json.Unmarshal(container.Levels, &rawLevels); err != nil {
		return nil, newProtocolError("malformed get_available_thinking_levels levels: %v", err)
	}
	levels := make([]ThinkingLevel, 0, len(rawLevels))
	seen := make(map[ThinkingLevel]bool, len(rawLevels))
	for i, raw := range rawLevels {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, newProtocolError("thinking level %d must be a string", i)
		}
		level, ok := ParseThinkingLevel(value)
		if !ok {
			return nil, newProtocolError("thinking level %d is unknown", i)
		}
		if seen[level] {
			return nil, newProtocolError("thinking level %d is duplicated", i)
		}
		seen[level] = true
		levels = append(levels, level)
	}
	return levels, nil
}

// SetThinkingLevel asks Pi to apply one exact supported level. A correlated,
// well-formed rejection is typed separately so worker policy can retain the
// previously confirmed default without treating transport failures as fallback.
func (c *Client) SetThinkingLevel(ctx context.Context, level ThinkingLevel) error {
	if parsed, ok := ParseThinkingLevel(string(level)); !ok || parsed != level {
		return fmt.Errorf("invalid thinking level %q", level)
	}
	req, err := newRequest(requestSetThinkingLevel)
	if err != nil {
		return err
	}
	req.Level = level
	resp, err := c.roundTrip(ctx, req)
	if err != nil {
		return err
	}
	if !*resp.Success {
		return &ThinkingLevelRejectedError{Level: level}
	}
	return nil
}

// GetState returns the exact active model and effective thinking level needed
// for post-activation confirmation. Every required projection is strict.
func (c *Client) GetState(ctx context.Context) (SessionState, error) {
	req, err := newRequest(requestGetState)
	if err != nil {
		return SessionState{}, err
	}
	resp, err := c.roundTrip(ctx, req)
	if err != nil {
		return SessionState{}, err
	}
	if !*resp.Success {
		return SessionState{}, &ReadinessError{Message: "get_state: " + responseDetail(resp)}
	}
	if len(resp.Data) == 0 || isJSONNull(resp.Data) {
		return SessionState{}, newProtocolError("get_state data must be an object with model and thinkingLevel")
	}
	var container struct {
		Model         json.RawMessage `json:"model"`
		ThinkingLevel json.RawMessage `json:"thinkingLevel"`
	}
	if err := json.Unmarshal(resp.Data, &container); err != nil {
		return SessionState{}, newProtocolError("malformed get_state data: %v", err)
	}
	if len(container.Model) == 0 || isJSONNull(container.Model) {
		return SessionState{}, newProtocolError("get_state data missing model object")
	}
	var model ModelProjection
	if err := json.Unmarshal(container.Model, &model); err != nil {
		return SessionState{}, newProtocolError("malformed get_state model: %v", err)
	}
	if _, ok := ExactModelSelector(model.Provider, model.ID); !ok {
		return SessionState{}, newProtocolError("get_state model has invalid provider or id")
	}
	if len(container.ThinkingLevel) == 0 || isJSONNull(container.ThinkingLevel) {
		return SessionState{}, newProtocolError("get_state data missing thinkingLevel")
	}
	var value string
	if err := json.Unmarshal(container.ThinkingLevel, &value); err != nil {
		return SessionState{}, newProtocolError("malformed get_state thinkingLevel: %v", err)
	}
	level, ok := ParseThinkingLevel(value)
	if !ok {
		return SessionState{}, newProtocolError("get_state thinkingLevel is unknown")
	}
	return SessionState{Model: model, ThinkingLevel: level}, nil
}

// Prompt submits one prompt message. A successful response only means Pi
// accepted the prompt; settlement is reported by the agent_settled event.
func (c *Client) Prompt(ctx context.Context, message string) error {
	req, err := newRequest(requestPrompt)
	if err != nil {
		return err
	}
	req.Message = message
	c.awaitingSettled = true
	c.settled = false
	resp, err := c.roundTrip(ctx, req)
	if err != nil {
		c.awaitingSettled = false
		return err
	}
	if !*resp.Success {
		c.awaitingSettled = false
		return &TaskError{Message: "prompt: " + responseDetail(resp)}
	}
	return nil
}

// GetLastAssistantText returns the final assistant text, or "" when Pi
// explicitly reports text:null. Pi serializes the undefined "no
// assistant text" value as an omitted text key (data:{}), so a missing
// text field is treated the same as an explicit null: empty text, never a
// protocol violation. A missing, null, or mistyped data object is still a
// protocol violation.
func (c *Client) GetLastAssistantText(ctx context.Context) (string, error) {
	req, err := newRequest(requestGetLastAssistantText)
	if err != nil {
		return "", err
	}
	resp, err := c.roundTrip(ctx, req)
	if err != nil {
		return "", err
	}
	if !*resp.Success {
		return "", &TaskError{Message: "get_last_assistant_text: " + responseDetail(resp)}
	}
	if len(resp.Data) == 0 || isJSONNull(resp.Data) {
		return "", newProtocolError("get_last_assistant_text data must be an object")
	}
	var data struct {
		Text json.RawMessage `json:"text"`
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return "", newProtocolError("malformed get_last_assistant_text data: %v", err)
	}
	if len(data.Text) == 0 {
		// Observed Pi 0.84.1 behavior: getLastAssistantText() returns
		// undefined when no assistant message exists or its text is empty,
		// and JSON serialization drops the undefined text key, producing
		// data:{}. The worker maps empty text to a task failure.
		return "", nil
	}
	if isJSONNull(data.Text) {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(data.Text, &text); err != nil {
		return "", newProtocolError("malformed get_last_assistant_text text: %v", err)
	}
	return text, nil
}

// WaitSettled blocks until the agent_settled event. agent_end is not
// terminal: retries, compaction, and queued work can follow it. A response
// frame while no request is outstanding is a protocol violation.
func (c *Client) WaitSettled(ctx context.Context) error {
	if c.settled {
		c.awaitingSettled = false
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			c.awaitingSettled = false
			return err
		}
		frame, err := c.nextFrame(ctx)
		if err != nil {
			c.awaitingSettled = false
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			return c.frameError(err)
		}
		isResponse, _, err := c.handleFrame(frame)
		if err != nil {
			c.awaitingSettled = false
			return err
		}
		if isResponse {
			c.awaitingSettled = false
			return newProtocolError("unexpected response while waiting for agent_settled")
		}
		if c.settled {
			c.awaitingSettled = false
			return nil
		}
	}
}

// roundTrip sends one request and waits for its correlated response,
// delivering interleaved events to the handler. It reports the RPC step as
// started, then completed or failed with its duration; request IDs are
// internal correlation only and never logged.
func (c *Client) roundTrip(ctx context.Context, req request) (*wireResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.ID == "" {
		req.ID = c.nextRequestID()
	}
	started := c.debug.Elapsed()
	c.debug.Log("rpc="+req.Type, debugStarted)
	if err := c.out.WriteFrame(req); err != nil {
		return nil, c.failRPC(req.Type, started, newTransportError(err))
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, c.failRPC(req.Type, started, err)
		}
		frame, err := c.nextFrame(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, c.failRPC(req.Type, started, err)
			}
			return nil, c.failRPC(req.Type, started, c.frameError(err))
		}
		isResponse, resp, err := c.handleFrame(frame)
		if err != nil {
			return nil, c.failRPC(req.Type, started, err)
		}
		if !isResponse {
			continue
		}
		if resp.ID != req.ID {
			return nil, c.failRPC(req.Type, started, newProtocolError("response for unknown request id %q (expected %q)", resp.ID, req.ID))
		}
		if resp.Command != req.Type {
			return nil, c.failRPC(req.Type, started, newProtocolError("response command %q does not match request type %q", resp.Command, req.Type))
		}
		duration := "duration=" + (c.debug.Elapsed() - started).Round(time.Millisecond).String()
		if !*resp.Success {
			// The RPC completed on the wire but failed semantically; the
			// caller classifies the typed failure.
			c.debug.Log("rpc="+req.Type, debugFailed, duration)
			return resp, nil
		}
		c.debug.Log("rpc="+req.Type, debugCompleted, duration)
		return resp, nil
	}
}

// failRPC logs the failed RPC step with its duration and returns err.
func (c *Client) failRPC(kind string, started time.Duration, err error) error {
	c.debug.Log("rpc="+kind, debugFailed, "duration="+(c.debug.Elapsed()-started).Round(time.Millisecond).String())
	return err
}

func (c *Client) nextFrame(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-c.frameResults:
		if result.err != nil {
			return nil, result.err
		}
		return result.frame, nil
	}
}

func (c *Client) pumpFrames() {
	defer close(c.done)
	for {
		frame, err := c.in.ReadFrame()
		result := frameReadResult{frame: frame, err: err}
		select {
		case <-c.stop:
			return
		case c.frameResults <- result:
		}
		if err != nil {
			return
		}
	}
}

// Close stops the frame pump and, when possible, closes the underlying
// reader so the pump can exit deterministically.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		close(c.stop)
		if c.closer != nil {
			c.closeErr = c.closer.Close()
		}
		<-c.done
	})
	return c.closeErr
}

// handleFrame routes one frame: responses are validated structurally, every
// other frame is delivered as an event.
func (c *Client) handleFrame(frame []byte) (isResponse bool, resp *wireResponse, err error) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(frame, &head); err != nil {
		return false, nil, newProtocolError("malformed frame: %v", err)
	}
	if head.Type == "" {
		return false, nil, newProtocolError("frame missing type")
	}
	if head.Type != "response" {
		event := Event{Type: head.Type, Raw: append(json.RawMessage(nil), frame...)}
		if head.Type == eventAgentSettled {
			if c.awaitingSettled && !c.settled {
				c.settled = true
				c.debugEvent(head.Type, frame)
			}
		} else {
			c.debugEvent(head.Type, frame)
		}
		if c.handler != nil {
			if err := c.handler.OnEvent(event); err != nil {
				return false, nil, newProtocolError("event handler rejected %q event: %v", head.Type, err)
			}
		}
		return false, nil, nil
	}
	var response wireResponse
	if err := json.Unmarshal(frame, &response); err != nil {
		return false, nil, newProtocolError("malformed response frame: %v", err)
	}
	if response.Success == nil {
		return false, nil, newProtocolError("response missing success field")
	}
	return true, &response, nil
}

// unknownName is the fixed projection for Pi-controlled tool names that
// are not on the fixed allowlist. The original values are never logged.
const unknownName = "unknown"

// allowedToolNames is the fixed built-in tool set from
// docs/pi-cli-surface.md. Tool names are Pi-controlled untrusted input:
// any other name is projected as the fixed word "unknown".
var allowedToolNames = map[string]bool{
	"read":  true,
	"bash":  true,
	"edit":  true,
	"write": true,
	"grep":  true,
	"find":  true,
	"ls":    true,
}

// debugEvent projects one inbound event into the worker debug stream. The
// switch below is the fixed event vocabulary: message_update inspects only
// assistantMessageEvent.type and updates the fixed model phase;
// tool_execution_start and tool_execution_end report only the allowlisted
// tool name, fixed status, duration, and the fixed bash failure cause
// projection; agent_settled reports settlement. agent_start/end,
// turn_start/end, message_start/end, tool_execution_update, and every other
// Pi-controlled type are suppressed entirely. No content, deltas, chunk or
// message counts, tool arguments, results, or raw frames are ever logged.
func (c *Client) debugEvent(eventType string, frame []byte) {
	switch eventType {
	case "message_update":
		var projection struct {
			AssistantMessageEvent struct {
				Type string `json:"type"`
			} `json:"assistantMessageEvent"`
		}
		// Only assistantMessageEvent.type is part of the phase projection;
		// all other message_update fields remain opaque. A malformed frame
		// cannot provide a subtype and therefore maps to model-activity.
		phase := debugModelActivity
		if err := json.Unmarshal(frame, &projection); err == nil {
			phase = modelPhase(projection.AssistantMessageEvent.Type)
		}
		c.modelUpdate(phase)
	case "tool_execution_start":
		var projection struct {
			ToolCallID string `json:"toolCallId"`
			ToolName   string `json:"toolName"`
		}
		// Malformed and unknown fields are opaque: a bad projection simply
		// omits the tool metadata, and the internal call id is only ever
		// used to correlate the completion duration, never logged.
		_ = json.Unmarshal(frame, &projection)
		if c.toolStarts == nil {
			c.toolStarts = make(map[string]time.Duration)
		}
		// Track only non-empty ids within the fixed byte bound and count
		// cap: an empty id cannot identify a specific call (every
		// empty-id completion would collide onto one entry and produce
		// false durations), an oversized id could retain tens of MiB of
		// untrusted strings across the cap, and once the cap is full new
		// starts still emit their safe started line untracked, so their
		// completions omit the duration. A start of an id that is already
		// tracked refreshes its timestamp even at cap, so the completion
		// reports the current call's duration instead of a stale one.
		if projection.ToolCallID != "" && len(projection.ToolCallID) <= toolCallIDMaxBytes {
			if _, tracked := c.toolStarts[projection.ToolCallID]; tracked || len(c.toolStarts) < toolStartCap {
				c.toolStarts[projection.ToolCallID] = c.debug.Elapsed()
			}
		}
		c.debug.Log("tool="+toolName(projection.ToolName), debugStarted)
	case "tool_execution_end":
		var projection struct {
			ToolCallID string `json:"toolCallId"`
			ToolName   string `json:"toolName"`
			IsError    bool   `json:"isError"`
		}
		_ = json.Unmarshal(frame, &projection)
		status := debugCompleted
		if projection.IsError {
			status = debugFailed
		}
		fields := []string{status}
		if started, ok := c.toolStarts[projection.ToolCallID]; ok {
			delete(c.toolStarts, projection.ToolCallID)
			fields = append(fields, "duration="+(c.debug.Elapsed()-started).Round(time.Millisecond).String())
		}
		if projection.IsError {
			cause, exitCode := bashFailureCause(projection.ToolName, frame)
			fields = append(fields, "cause="+cause)
			if exitCode != "" {
				fields = append(fields, "exit-code="+exitCode)
			}
		}
		c.debug.Log("tool="+toolName(projection.ToolName), fields...)
	case "agent_settled":
		c.debug.LogTerminal(debugSettled)
	case "tool_execution_update":
		// Suppressed: one line per output chunk would flood --debug.
	default:
		// agent_start, agent_end, turn_start, turn_end, message_start,
		// message_end, and every unknown Pi-controlled event type are
		// suppressed entirely and never logged verbatim.
	}
}

// toolName maps a Pi-controlled tool name onto the fixed allowlist; any
// other name is projected as the fixed word "unknown" and never logged
// verbatim.
func toolName(name string) string {
	if !allowedToolNames[name] {
		return unknownName
	}
	return name
}

// modelPhase maps only the assistantMessageEvent.type vocabulary that Pi
// emits. Missing, malformed, and unknown subtypes intentionally share
// model-activity; no other frame field participates in this projection.
func modelPhase(subtype string) string {
	switch subtype {
	case "thinking_start", "thinking_delta", "thinking_end":
		return debugModelThinking
	case "text_start", "text_delta", "text_end":
		return debugModelOutput
	case "toolcall_start", "toolcall_delta", "toolcall_end":
		return debugModelToolCall
	default:
		return debugModelActivity
	}
}

// modelUpdate emits immediately when the fixed model phase changes. Repeated
// events in the same phase are suppressed; the lifecycle heartbeat is the
// only repeated liveness clock.
func (c *Client) modelUpdate(phase string) {
	if phase == c.modelPhase {
		return
	}
	c.modelPhase = phase
	c.debug.Log(phase, "elapsed="+c.debug.Elapsed().Round(time.Millisecond).String())
}

// bashFailureCause projects only the final text entry of result.content for
// an exact bash failure. It never returns any upstream text; exitCode is the
// only value copied into a debug field, and only after strict validation.
func bashFailureCause(tool string, frame []byte) (cause, exitCode string) {
	if tool != "bash" {
		return "unknown", ""
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil || len(envelope.Result) == 0 || isJSONNull(envelope.Result) {
		return "unknown", ""
	}
	var result struct {
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(envelope.Result, &result); err != nil || result.Content == nil || len(result.Content) == 0 {
		return "unknown", ""
	}
	var entry struct {
		Type string          `json:"type"`
		Text json.RawMessage `json:"text"`
	}
	if err := json.Unmarshal(result.Content[len(result.Content)-1], &entry); err != nil || entry.Type != "text" || len(entry.Text) == 0 || isJSONNull(entry.Text) {
		return "unknown", ""
	}
	var text string
	if err := json.Unmarshal(entry.Text, &text); err != nil {
		return "unknown", ""
	}
	status, ok := finalBashStatus(text)
	if !ok {
		return "unknown", ""
	}
	if strings.HasPrefix(status, "Command exited with code ") {
		value := strings.TrimPrefix(status, "Command exited with code ")
		if isPositiveDecimal(value) {
			return "nonzero-exit", value
		}
		return "unknown", ""
	}
	if strings.HasPrefix(status, "Command timed out after ") && strings.HasSuffix(status, " seconds") {
		value := strings.TrimSuffix(strings.TrimPrefix(status, "Command timed out after "), " seconds")
		if isDecimal(value) {
			return "timeout", ""
		}
		return "unknown", ""
	}
	if status == "Command aborted" || status == "Operation aborted" {
		return "cancelled", ""
	}
	return "unknown", ""
}

// finalBashStatus accepts the exact status text Pi appends, either as the
// complete output or after its fixed blank-line separator. Arbitrary output
// before the suffix is deliberately discarded and never returned.
func finalBashStatus(text string) (string, bool) {
	if (strings.HasPrefix(text, "Command ") || text == "Operation aborted") && !strings.Contains(text, "\n\n") {
		return text, true
	}
	if index := strings.LastIndex(text, "\n\n"); index >= 0 && index+2 < len(text) {
		status := text[index+2:]
		if !strings.Contains(status, "\n") {
			return status, true
		}
	}
	return "", false
}

func isPositiveDecimal(value string) bool {
	if !isDecimal(value) || value[0] == '0' {
		return false
	}
	n, err := strconv.ParseUint(value, 10, 64)
	return err == nil && n > 0
}

func isDecimal(value string) bool {
	if value == "" {
		return false
	}
	dot := false
	for i := 0; i < len(value); i++ {
		switch {
		case value[i] >= '0' && value[i] <= '9':
		case value[i] == '.' && !dot:
			dot = true
		default:
			return false
		}
	}
	return value[0] != '.' && value[len(value)-1] != '.'
}

// frameError maps framing failures onto deterministic errors.
func (c *Client) frameError(err error) error {
	switch {
	case errors.Is(err, errEmptyFrame):
		return newProtocolError("empty frame")
	case errors.Is(err, ErrFrameTooLarge):
		return newProtocolError("oversized rpc frame (limit %d bytes)", MaxFrameBytes)
	case errors.Is(err, io.EOF):
		return newTransportError(err)
	default:
		return newTransportError(err)
	}
}

// isJSONNull reports whether raw is the JSON null literal, ignoring
// surrounding whitespace. It is used to distinguish an explicitly null
// container from a missing one in success response validation.
func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// responseDetail returns the pi-provided failure detail, or a fixed
// placeholder when the failure carried none.
func responseDetail(resp *wireResponse) string {
	if resp.Error != "" {
		return resp.Error
	}
	return "pi returned no error detail"
}

package pi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

const eventAgentSettled = "agent_settled"

// EventHandler receives every non-response frame. A handler error is treated
// as a protocol violation and fails the client deterministically.
type EventHandler interface {
	OnEvent(Event) error
}

// streamingHeartbeatInterval bounds model-streaming debug lines by elapsed
// run time: after the first message_update of a run logs one activity line,
// later updates log at most one heartbeat per interval, independent of
// frame count.
const streamingHeartbeatInterval = 30 * time.Second

// toolStartCap bounds the tool-execution start timings the client retains
// for completion-duration correlation. Tool call ids are Pi-controlled
// untrusted input, so an unmatched flood of unique ids must not grow memory
// without bound. The cap is one eighth of the debug line budget, which
// already bounds the debug lines such starts can emit. Once the cap is
// reached, new starts still emit their safe started line but are not
// tracked, so their completions report no duration instead of a false one.
const toolStartCap = debugLineBudget / 8

// toolCallIDMaxBytes bounds one retained tool-call id in bytes. Pi controls
// the id and one frame may approach the 1 MiB record limit, so an id is
// only eligible for timing correlation when it fits this small fixed bound:
// oversized and empty ids are never retained, their safe start/end lines
// still appear, and their completions omit the duration. The id is only
// ever used as a map key, never logged.
const toolCallIDMaxBytes = 128

// Client drives the four documented outbound RPC request types over a Pi
// JSONL stream. Requests carry generated IDs; responses are correlated by ID
// while events interleave. The client is single-flight: one request at a
// time, matching the worker's linear prompt lifecycle. There is no arbitrary
// caller JSON API and no direct RPC bash surface.
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
	// streamed records that the first model-activity line was emitted;
	// lastBeat is the run elapsed time of the last emitted model-streaming
	// line. Only the single driving goroutine touches them.
	streamed bool
	lastBeat time.Duration
	// toolStarts correlates tool_execution_start/end timings by the
	// internal tool-call identifier, bounded by toolStartCap. The
	// identifier is Pi-controlled untrusted input and is only ever used
	// as this map key, never logged. Only the single driving goroutine
	// touches it.
	toolStarts map[string]time.Duration
}

func NewClient(stdin io.Writer, stdout io.Reader, handler EventHandler, debug *WorkerScope) *Client {
	return &Client{
		in:      NewFrameReader(stdout, MaxFrameBytes),
		out:     NewFrameWriter(stdin),
		handler: handler,
		debug:   debug,
	}
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
			return nil, &ReadinessError{Message: "pi exited before returning the model catalog; verify Pi 0.84.1 compatibility and provider login"}
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
	for i, model := range data.Models {
		if model.Provider == "" || model.ID == "" {
			return nil, newProtocolError("catalog entry %d missing provider or id", i)
		}
	}
	return data.Models, nil
}

// SetModel activates the exact catalog provider/id. Pi 0.84.1 confirms a
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
// explicitly reports text:null. Pi 0.84.1 serializes the undefined "no
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
		frame, err := c.in.ReadFrame()
		if err != nil {
			c.awaitingSettled = false
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
		frame, err := c.in.ReadFrame()
		if err != nil {
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

// handleFrame routes one frame: responses are validated structurally, every
// other frame is delivered as an event.
func (c *Client) handleFrame(frame []byte) (isResponse bool, resp *wireResponse, err error) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(frame, &head); err != nil {
		return false, nil, newProtocolError("malformed frame: %v", err)
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
// switch below is the fixed event vocabulary: message_update drives the
// elapsed-time streaming heartbeat; tool_execution_start and
// tool_execution_end report only the allowlisted tool name and a fixed
// status, plus the duration on completion; agent_settled reports
// settlement. agent_start/end, turn_start/end, message_start/end,
// tool_execution_update, and every other Pi-controlled type are suppressed
// entirely. No content, deltas, chunk or message counts, tool arguments,
// results, or raw frames are ever logged.
func (c *Client) debugEvent(eventType string, frame []byte) {
	switch eventType {
	case "message_update":
		c.streamingUpdate()
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
		c.debug.Log("tool="+toolName(projection.ToolName), fields...)
	case "agent_settled":
		c.debug.Log(debugSettled)
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

// streamingUpdate bounds model-streaming lines by elapsed run time: the
// first message_update of the run logs one activity line, and later
// updates log at most one heartbeat per streamingHeartbeatInterval,
// independent of frame count.
func (c *Client) streamingUpdate() {
	elapsed := c.debug.Elapsed().Round(time.Millisecond)
	if !c.streamed {
		c.streamed = true
		c.lastBeat = elapsed
		c.debug.Log(debugStreaming, "elapsed="+elapsed.String())
		return
	}
	if elapsed-c.lastBeat >= streamingHeartbeatInterval {
		c.lastBeat = elapsed
		c.debug.Log(debugStreaming, "elapsed="+elapsed.String())
	}
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

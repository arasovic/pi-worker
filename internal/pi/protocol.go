// Package pi implements the pi-worker v0 foreground worker: it launches the
// host Pi 0.84.1 executable in RPC mode and drives it through the four
// documented outbound JSONL request types.
package pi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxFrameBytes bounds one JSONL record in either direction. Pi messages are
// small; this bound keeps malformed or hostile peers from exhausting memory.
const MaxFrameBytes = 1 << 20

// ErrFrameTooLarge reports a JSONL record that exceeds the configured bound.
var ErrFrameTooLarge = errors.New("frame exceeds maximum record size")

// errEmptyFrame marks a parsed but empty JSONL record. It keeps empty frames
// as protocol violations instead of transport EOF.
var errEmptyFrame = errors.New("empty frame")

// transportError reports a transport-level RPC stream failure without exposing
// the underlying stream detail in its public error text.
//
// The wrapped error is available through Unwrap for internal classification and
// testing.
type transportError struct {
	err error
}

func (e *transportError) Error() string {
	return "pi rpc stream failed"
}

func (e *transportError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func newTransportError(err error) error {
	return &transportError{err: err}
}

// ProtocolError reports a deterministic RPC protocol violation: malformed
// JSON, empty or oversized frames, unknown request IDs, or a response that does
// not match its request.
type ProtocolError struct {
	Message string
}

func (e *ProtocolError) Error() string { return "protocol error: " + e.Message }

func newProtocolError(format string, args ...any) error {
	return &ProtocolError{Message: fmt.Sprintf(format, args...)}
}

// ModelUnavailableError reports that the requested exact provider/model
// selector is not present in the available-model catalog.
type ModelUnavailableError struct {
	Model string
}

func (e *ModelUnavailableError) Error() string {
	return fmt.Sprintf("model not available: %s", e.Model)
}

// ReadinessError reports that Pi or its provider/auth state rejected the
// worker before or during model activation.
type ReadinessError struct {
	Message string
}

func (e *ReadinessError) Error() string { return "pi not ready: " + e.Message }

// TaskError reports that the submitted prompt was rejected or produced no
// usable result.
type TaskError struct {
	Message string
}

func (e *TaskError) Error() string { return "task failed: " + e.Message }

// Outbound RPC request types. Pi-worker emits exactly these four request
// shapes and rejects every other RPC type, in particular direct RPC bash.
// The observed upstream get_state and abort commands are deferred until a
// lifecycle consumer needs them.
const (
	requestGetAvailableModels   = "get_available_models"
	requestSetModel             = "set_model"
	requestPrompt               = "prompt"
	requestGetLastAssistantText = "get_last_assistant_text"
)

var allowedRequestTypes = map[string]bool{
	requestGetAvailableModels:   true,
	requestSetModel:             true,
	requestPrompt:               true,
	requestGetLastAssistantText: true,
}

// request is the closed outbound envelope. Only the four documented request
// types may be constructed; callers never supply raw RPC JSON.
type request struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type"`
	Provider string `json:"provider,omitempty"`
	ModelID  string `json:"modelId,omitempty"`
	Message  string `json:"message,omitempty"`
}

// newRequest constructs a request of one of the four documented types and
// rejects every other RPC type, keeping the outbound surface closed.
func newRequest(kind string) (request, error) {
	if !allowedRequestTypes[kind] {
		return request{}, fmt.Errorf("disallowed rpc request type %q", kind)
	}
	return request{Type: kind}, nil
}

// ModelProjection is the v0 projection of one available-model catalog entry.
// Every other catalog field is ignored and never re-serialized.
type ModelProjection struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
}

// Event is a non-response frame from Pi. Type is the frame type; Raw carries
// the full server frame verbatim for later projections.
type Event struct {
	Type string
	Raw  json.RawMessage
}

// wireResponse is the v0 projection of a response frame.
type wireResponse struct {
	ID      string          `json:"id"`
	Command string          `json:"command"`
	Success *bool           `json:"success"`
	Error   string          `json:"error"`
	Data    json.RawMessage `json:"data"`
}

// FrameReader reads LF-delimited JSONL records from an RPC stream with a
// bounded record size. Trailing carriage returns are tolerated per the
// protocol documentation.
type FrameReader struct {
	br  *bufio.Reader
	max int
}

func NewFrameReader(r io.Reader, max int) *FrameReader {
	return &FrameReader{br: bufio.NewReader(r), max: max}
}

// ReadFrame returns the next record without its terminating LF. It returns
// io.EOF at a clean or truncated end of stream and ErrFrameTooLarge for a
// record that exceeds the configured bound.
func (r *FrameReader) ReadFrame() ([]byte, error) {
	var record []byte
	for {
		chunk, err := r.br.ReadSlice('\n')
		record = append(record, chunk...)
		if len(record) > r.max {
			return nil, ErrFrameTooLarge
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return nil, err
		}
		break
	}
	record = bytes.TrimSuffix(record, []byte("\n"))
	record = bytes.TrimSuffix(record, []byte("\r"))
	if len(record) == 0 {
		return nil, errEmptyFrame
	}
	return record, nil
}

// FrameWriter writes LF-delimited JSONL records.
type FrameWriter struct {
	bw *bufio.Writer
}

func NewFrameWriter(w io.Writer) *FrameWriter {
	return &FrameWriter{bw: bufio.NewWriter(w)}
}

// WriteFrame marshals v as one JSONL record and flushes it.
func (w *FrameWriter) WriteFrame(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode frame: %w", err)
	}
	if _, err := w.bw.Write(data); err != nil {
		return err
	}
	if err := w.bw.WriteByte('\n'); err != nil {
		return err
	}
	return w.bw.Flush()
}

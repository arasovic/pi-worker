package pi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestNewRequestAcceptsOnlyTheFourDocumentedTypes(t *testing.T) {
	documented := []string{
		"get_available_models",
		"set_model",
		"prompt",
		"get_last_assistant_text",
	}
	for _, kind := range documented {
		req, err := newRequest(kind)
		if err != nil {
			t.Fatalf("newRequest(%q) = %v", kind, err)
		}
		if req.Type != kind {
			t.Fatalf("newRequest(%q).Type = %q", kind, req.Type)
		}
	}
	disallowed := []string{"bash", "response", "event", "get_prompt_templates", "get_state", "abort", ""}
	for _, kind := range disallowed {
		if _, err := newRequest(kind); err == nil {
			t.Fatalf("newRequest(%q) succeeded, want rejection", kind)
		}
	}
}

func TestRequestEnvelopeMarshalsOnlyDocumentedFields(t *testing.T) {
	setModel := request{ID: "r1", Type: "set_model", Provider: "acme", ModelID: "m-1"}
	data, err := json.Marshal(setModel)
	if err != nil {
		t.Fatalf("marshal set_model: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("unmarshal set_model: %v", err)
	}
	want := map[string]any{"id": "r1", "type": "set_model", "provider": "acme", "modelId": "m-1"}
	if len(payload) != len(want) {
		t.Fatalf("set_model payload = %v, want %v", payload, want)
	}
	for key, value := range want {
		if payload[key] != value {
			t.Fatalf("set_model payload[%q] = %v, want %v", key, payload[key], value)
		}
	}

	prompt := request{ID: "r2", Type: "prompt", Message: "hello"}
	data, err = json.Marshal(prompt)
	if err != nil {
		t.Fatalf("marshal prompt: %v", err)
	}
	payload = nil
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("unmarshal prompt: %v", err)
	}
	want = map[string]any{"id": "r2", "type": "prompt", "message": "hello"}
	if len(payload) != len(want) {
		t.Fatalf("prompt payload = %v, want %v", payload, want)
	}
	for key, value := range want {
		if payload[key] != value {
			t.Fatalf("prompt payload[%q] = %v, want %v", key, payload[key], value)
		}
	}
}

func TestFrameRoundTripUsesLFJSONLFraming(t *testing.T) {
	var stream bytes.Buffer
	writer := NewFrameWriter(&stream)
	if err := writer.WriteFrame(map[string]any{"type": "prompt"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := writer.WriteFrame(request{Type: "get_last_assistant_text"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	lines := strings.Split(stream.String(), "\n")
	if len(lines) != 3 || lines[2] != "" {
		t.Fatalf("framing = %q", stream.String())
	}

	reader := NewFrameReader(&stream, MaxFrameBytes)
	first, err := reader.ReadFrame()
	if err != nil {
		t.Fatalf("read first: %v", err)
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(first, &head); err != nil || head.Type != "prompt" {
		t.Fatalf("first frame = %s, err %v", first, err)
	}
	second, err := reader.ReadFrame()
	if err != nil {
		t.Fatalf("read second: %v", err)
	}
	if string(second) != `{"type":"get_last_assistant_text"}` {
		t.Fatalf("second frame = %q", second)
	}
}

func TestFrameReaderToleratesTrailingCarriageReturn(t *testing.T) {
	reader := NewFrameReader(strings.NewReader("{\"a\":1}\r\n"), MaxFrameBytes)
	frame, err := reader.ReadFrame()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(frame) != `{"a":1}` {
		t.Fatalf("frame = %q", frame)
	}
}

func TestFrameReaderRejectsOversizedRecords(t *testing.T) {
	reader := NewFrameReader(strings.NewReader(strings.Repeat("x", 100)+"\n"), 50)
	if _, err := reader.ReadFrame(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
}

func TestFrameReaderRejectsEmptyRecords(t *testing.T) {
	reader := NewFrameReader(strings.NewReader("\n"), MaxFrameBytes)
	if _, err := reader.ReadFrame(); err == nil {
		t.Fatalf("empty frame accepted")
	}
}

func TestFrameReaderReportsEOF(t *testing.T) {
	reader := NewFrameReader(strings.NewReader(""), MaxFrameBytes)
	if _, err := reader.ReadFrame(); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

func TestFrameReaderReportsTruncatedFrameAsEOF(t *testing.T) {
	reader := NewFrameReader(strings.NewReader(`{"type":"agent`), MaxFrameBytes)
	if _, err := reader.ReadFrame(); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

func TestTypedErrors(t *testing.T) {
	if got := (&ProtocolError{Message: "boom"}).Error(); !strings.Contains(got, "protocol") || !strings.Contains(got, "boom") {
		t.Fatalf("ProtocolError message = %q", got)
	}
	if got := (&ModelUnavailableError{Model: "acme/m-1"}).Error(); !strings.Contains(got, "acme/m-1") {
		t.Fatalf("ModelUnavailableError message = %q", got)
	}
	if got := (&ReadinessError{Message: "auth"}).Error(); !strings.Contains(got, "auth") {
		t.Fatalf("ReadinessError message = %q", got)
	}
	if got := (&TaskError{Message: "no text"}).Error(); !strings.Contains(got, "no text") {
		t.Fatalf("TaskError message = %q", got)
	}
	var protocolErr *ProtocolError
	if !errors.As(newProtocolError("x"), &protocolErr) {
		t.Fatalf("newProtocolError is not a *ProtocolError")
	}
}

func TestMaxFrameBytesBoundsRecords(t *testing.T) {
	if MaxFrameBytes <= 0 {
		t.Fatalf("MaxFrameBytes = %d", MaxFrameBytes)
	}
}

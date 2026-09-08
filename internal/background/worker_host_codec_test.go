package background

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/pi"
)

// validWorkerHostRequest returns one wire-valid execution request: a
// positive worker identity, a real workspace, a provider/id model, no
// thinking level, a non-blank prompt, a pi executable, and a positive
// execution timeout.
func validWorkerHostRequest() workerHostRequest {
	return workerHostRequest{
		workerID:         1,
		workspace:        "ws",
		model:            "acme/m-1",
		prompt:           "execute the focused task",
		piExecutable:     "/usr/bin/pi",
		executionTimeout: 5 * time.Minute,
	}
}

// marshalAnyJSON marshals v into a compact JSON document for wire
// fixtures.
func marshalAnyJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal wire fixture: %v", err)
	}
	return data
}

// decodeWorkerHostRequestRaw is decodeWorkerHostRequest over raw bytes.
func decodeWorkerHostRequestRaw(t *testing.T, data []byte) (workerHostRequest, error) {
	t.Helper()
	return decodeWorkerHostRequest(data)
}

// TestWorkerHostRequestRoundTrip verifies that a valid request encodes
// and decodes to the identical domain request, with every field intact.
func TestWorkerHostRequestRoundTrip(t *testing.T) {
	req := validWorkerHostRequest()
	req.thinkingLevel = pi.ThinkingMax
	req.workerID = 3

	data, err := encodeWorkerHostRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodeWorkerHostRequest(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != req {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, req)
	}
	// The document is exactly one JSON object: no trailing newline or
	// data.
	if !strings.HasPrefix(string(data), "{") || !strings.HasSuffix(string(data), "}") {
		t.Fatalf("wire document %q is not a single JSON object", data)
	}
}

// TestWorkerHostRequestValidationFailures drives every validation rule
// through encode, which must reject before any payload exists.
func TestWorkerHostRequestValidationFailures(t *testing.T) {
	base := validWorkerHostRequest()
	tests := []struct {
		name    string
		mutate  func(*workerHostRequest)
		wantErr string
	}{
		{"zero worker id", func(r *workerHostRequest) { r.workerID = 0 }, "workerId must be positive"},
		{"negative worker id", func(r *workerHostRequest) { r.workerID = -1 }, "workerId must be positive"},
		{"empty workspace", func(r *workerHostRequest) { r.workspace = "" }, "workspace is required"},
		{"empty model", func(r *workerHostRequest) { r.model = "" }, "model is required"},
		{"model without provider", func(r *workerHostRequest) { r.model = "no-slash" }, "provider/id"},
		{"model with empty provider", func(r *workerHostRequest) { r.model = "/id" }, "provider/id"},
		{"model with empty id", func(r *workerHostRequest) { r.model = "provider/" }, "provider/id"},
		{"invalid thinking level", func(r *workerHostRequest) { r.thinkingLevel = "turbo" }, "thinkingLevel is not a valid Pi thinking level"},
		{"blank prompt", func(r *workerHostRequest) { r.prompt = "   " }, "prompt must not be blank"},
		{"empty prompt", func(r *workerHostRequest) { r.prompt = "" }, "prompt must not be blank"},
		{"empty pi executable", func(r *workerHostRequest) { r.piExecutable = "" }, "piExecutable is required"},
		{"zero execution timeout", func(r *workerHostRequest) { r.executionTimeout = 0 }, "executionTimeout must be positive"},
		{"negative execution timeout", func(r *workerHostRequest) { r.executionTimeout = -time.Second }, "executionTimeout must be positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := base
			tt.mutate(&req)
			_, err := encodeWorkerHostRequest(req)
			if err == nil {
				t.Fatal("encode succeeded, want validation failure")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestWorkerHostRequestDecodeRejectsMalformedDocuments drives the strict
// wire-level rules: wrong schema version, unknown fields, trailing data,
// invalid UTF-8, a missing or malformed executionTimeout, and an invalid
// payload must all fail before any host could run.
func TestWorkerHostRequestDecodeRejectsMalformedDocuments(t *testing.T) {
	valid := marshalAnyJSON(t, workerHostRequestJSON{
		SchemaVersion:    workerHostSchemaVersion,
		WorkerID:         1,
		Workspace:        "ws",
		Model:            "acme/m-1",
		Prompt:           "task",
		PiExecutable:     "/usr/bin/pi",
		ExecutionTimeout: "5m",
	})
	decoded, err := decodeWorkerHostRequestRaw(t, valid)
	if err != nil || decoded.workerID != 1 || decoded.model != "acme/m-1" || decoded.executionTimeout != 5*time.Minute {
		t.Fatalf("valid wire document did not decode: %+v, %v", decoded, err)
	}

	validMap := map[string]any{
		"schemaVersion":    workerHostSchemaVersion,
		"workerId":         1,
		"workspace":        "ws",
		"model":            "acme/m-1",
		"prompt":           "task",
		"piExecutable":     "/usr/bin/pi",
		"executionTimeout": "5m",
	}
	tests := []struct {
		name    string
		mutate  func(map[string]any)
		wantErr string
	}{
		{"wrong schema version", func(m map[string]any) { m["schemaVersion"] = 99 }, "schemaVersion must be 1"},
		{"unknown field", func(m map[string]any) { m["sneaky"] = 1 }, "unknown field"},
		{"trailing data", func(m map[string]any) {}, ""},
		{"missing workerId", func(m map[string]any) { delete(m, "workerId") }, "workerId must be positive"},
		{"zero workerId", func(m map[string]any) { m["workerId"] = 0 }, "workerId must be positive"},
		{"missing workspace", func(m map[string]any) { delete(m, "workspace") }, "workspace is required"},
		{"missing model", func(m map[string]any) { delete(m, "model") }, "model is required"},
		{"missing prompt", func(m map[string]any) { delete(m, "prompt") }, "prompt must not be blank"},
		{"missing piExecutable", func(m map[string]any) { delete(m, "piExecutable") }, "piExecutable is required"},
		{"missing executionTimeout", func(m map[string]any) { delete(m, "executionTimeout") }, "not a valid duration"},
		{"malformed executionTimeout", func(m map[string]any) { m["executionTimeout"] = "soon" }, "not a valid duration"},
		{"zero executionTimeout", func(m map[string]any) { m["executionTimeout"] = "0s" }, "executionTimeout must be positive"},
		{"invalid thinking", func(m map[string]any) { m["thinkingLevel"] = "turbo" }, "thinkingLevel"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := make(map[string]any, len(validMap)+1)
			for k, v := range validMap {
				m[k] = v
			}
			if tt.name == "trailing data" {
				data := append(marshalAnyJSON(t, m), []byte(" {}")...)
				_, err := decodeWorkerHostRequestRaw(t, data)
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("decode = %v, want trailing-data failure", err)
				}
				return
			}
			tt.mutate(m)
			_, err := decodeWorkerHostRequestRaw(t, marshalAnyJSON(t, m))
			if err == nil {
				t.Fatal("decode succeeded, want strict rejection")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
	// Invalid UTF-8 is rejected before JSON decoding.
	if _, err := decodeWorkerHostRequestRaw(t, []byte{0xff, 0xfe, 0x00, 0x01}); err == nil || !strings.Contains(err.Error(), "not valid UTF-8") {
		t.Fatalf("invalid UTF-8 decode = %v", err)
	}
}

// TestWorkerHostResponseProcessStartRoundTrip verifies process-start
// frame encoding and strict decoding.
func TestWorkerHostResponseProcessStartRoundTrip(t *testing.T) {
	data, err := encodeWorkerHostProcessStart(2, 4242)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	frame, err := decodeWorkerHostResponse(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if frame.kind != workerHostFrameProcessStart || frame.workerID != 2 || frame.pid != 4242 {
		t.Fatalf("decoded frame = %+v, want process-start worker 2 pid 4242", frame)
	}

	for _, tt := range []struct {
		name     string
		workerID int
		pid      int
	}{
		{"zero worker id", 0, 42},
		{"negative worker id", -1, 42},
		{"zero pid", 1, 0},
		{"negative pid", 1, -5},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := encodeWorkerHostProcessStart(tt.workerID, tt.pid)
			if err == nil {
				t.Fatal("encode succeeded, want rejection")
			}
		})
	}
}

// TestWorkerHostResponseResultRoundTripPreservesFields verifies that a
// fully populated worker result survives the terminal frame round trip
// field for field.
func TestWorkerHostResponseResultRoundTripPreservesFields(t *testing.T) {
	result := pi.WorkerResult{
		Model:                  "acme/m-1",
		RequestedThinkingLevel: pi.ThinkingMax,
		ThinkingLevel:          pi.ThinkingMedium,
		ThinkingFallback:       true,
		Warning:                "startup retry warning",
		Explanation:            "the final answer",
		Status:                 pi.StatusCompleted,
		DataFiles: []pi.DataFile{
			{Path: "a.txt", Bytes: 12, SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		},
		Usage: &pi.Usage{Input: 10, Output: 5, TotalTokens: 15, Cost: pi.UsageCost{Total: 0.01}},
	}
	data, err := encodeWorkerHostResult(result)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	frame, err := decodeWorkerHostResponse(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if frame.kind != workerHostFrameResult {
		t.Fatalf("kind = %q, want result", frame.kind)
	}
	got := frame.result
	if got.Status != pi.StatusCompleted || got.Model != "acme/m-1" || got.Explanation != "the final answer" {
		t.Fatalf("terminal result identity mismatch: %+v", got)
	}
	if got.RequestedThinkingLevel != pi.ThinkingMax || got.ThinkingLevel != pi.ThinkingMedium || !got.ThinkingFallback {
		t.Fatalf("thinking metadata not preserved: %+v", got)
	}
	if got.Warning != "startup retry warning" || len(got.DataFiles) != 1 || got.DataFiles[0].Path != "a.txt" {
		t.Fatalf("warning/data files not preserved: %+v", got)
	}
	if got.Usage == nil || got.Usage.Input != 10 || got.Usage.Output != 5 || got.Usage.Cost.Total != 0.01 {
		t.Fatalf("usage not preserved: %+v", got.Usage)
	}
}

// TestWorkerHostResponseResultEmptyStatusRoundTripsButDecodeRejects
// verifies the decode-side terminal rule: a worker result always carries
// a defined status, so an absent or unknown status makes the frame
// invalid — encoding preserves it, decoding refuses it, and an invalid
// terminal result can never imply success.
func TestWorkerHostResponseResultEmptyStatusRoundTripsButDecodeRejects(t *testing.T) {
	data, err := encodeWorkerHostResult(pi.WorkerResult{Model: "acme/m-1"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := decodeWorkerHostResponse(data); err == nil {
		t.Fatal("decode succeeded for a result without a defined status")
	}
}

// TestWorkerHostResponseDecodeRejectsMalformedFrames drives the strict
// response-frame rules: wrong schema version, unknown fields, trailing
// data, invalid UTF-8, unknown kind, payload fields that do not belong
// to the kind, absent or null payloads, and invalid identities must all
// fail.
func TestWorkerHostResponseDecodeRejectsMalformedFrames(t *testing.T) {
	startFrame, err := encodeWorkerHostProcessStart(1, 42)
	if err != nil {
		t.Fatalf("encode start frame: %v", err)
	}
	if _, err := decodeWorkerHostResponse(startFrame); err != nil {
		t.Fatalf("valid start frame: %v", err)
	}

	tests := []struct {
		name    string
		doc     map[string]any
		wantErr string
	}{
		{"wrong schema version", map[string]any{"schemaVersion": 9, "kind": "result", "result": map[string]any{"model": "acme/m-1", "status": "completed"}}, "schemaVersion must be 1"},
		{"unknown field", map[string]any{"schemaVersion": workerHostResponseSchemaVersion, "kind": "result", "result": map[string]any{"model": "acme/m-1", "status": "completed"}, "extra": 1}, "unknown field"},
		{"unknown kind", map[string]any{"schemaVersion": workerHostResponseSchemaVersion, "kind": "telemetry"}, "unknown kind"},
		{"empty kind", map[string]any{"schemaVersion": workerHostResponseSchemaVersion}, "unknown kind"},
		{"process-start with result payload", map[string]any{"schemaVersion": workerHostResponseSchemaVersion, "kind": "process-start", "workerId": 1, "pid": 42, "result": map[string]any{"model": "acme/m-1", "status": "completed"}}, "must not carry a result payload"},
		{"process-start missing identity", map[string]any{"schemaVersion": workerHostResponseSchemaVersion, "kind": "process-start", "pid": 42}, "requires workerId and pid"},
		{"process-start zero worker id", map[string]any{"schemaVersion": workerHostResponseSchemaVersion, "kind": "process-start", "workerId": 0, "pid": 42}, "workerId must be positive"},
		{"process-start zero pid", map[string]any{"schemaVersion": workerHostResponseSchemaVersion, "kind": "process-start", "workerId": 1, "pid": 0}, "pid must be positive"},
		{"result with identity payload", map[string]any{"schemaVersion": workerHostResponseSchemaVersion, "kind": "result", "workerId": 1, "pid": 42, "result": map[string]any{"model": "acme/m-1", "status": "completed"}}, "must not carry a process identity payload"},
		{"result missing payload", map[string]any{"schemaVersion": workerHostResponseSchemaVersion, "kind": "result"}, "requires a result payload"},
		{"result null payload", map[string]any{"schemaVersion": workerHostResponseSchemaVersion, "kind": "result", "result": nil}, "requires a result payload"},
		{"result unknown payload field", map[string]any{"schemaVersion": workerHostResponseSchemaVersion, "kind": "result", "result": map[string]any{"model": "acme/m-1", "status": "completed", "sneaky": 1}}, "unknown field"},
		{"result invalid status", map[string]any{"schemaVersion": workerHostResponseSchemaVersion, "kind": "result", "result": map[string]any{"model": "acme/m-1", "status": "miraculous"}}, "not a defined worker status"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeWorkerHostResponse(marshalAnyJSON(t, tt.doc))
			if err == nil {
				t.Fatal("decode succeeded, want strict rejection")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}

	// Trailing data after a valid document is rejected.
	trailing := append(append([]byte{}, startFrame...), []byte(" {}")...)
	if _, err := decodeWorkerHostResponse(trailing); err == nil {
		t.Fatal("decode succeeded with trailing data")
	}
	// Invalid UTF-8 is rejected before JSON decoding.
	if _, err := decodeWorkerHostResponse([]byte{0xff, 0xfe}); err == nil || !strings.Contains(err.Error(), "not valid UTF-8") {
		t.Fatalf("invalid UTF-8 decode = %v", err)
	}
}

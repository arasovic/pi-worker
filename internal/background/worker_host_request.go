package background

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/arasovic/pi-worker/internal/pi"
)

// workerHostSchemaVersion is the only wire schema version accepted and
// produced for a worker-host execution request.
const workerHostSchemaVersion = 1

// workerHostRequest is one task handed to a private same-binary
// worker-host child: everything the child host needs to execute exactly
// one worker invocation in a private workspace without deriving anything
// from its parent. The prompt is already composed by the run/supervisor
// layer — the host carries no data metadata and no admission state, and
// it never enqueues a ticket.
type workerHostRequest struct {
	// workerID is the positive worker identity the parent assigned;
	// it labels process-start notifications only and never appears in
	// results or JSON documents beyond this private wire.
	workerID         int
	workspace        string
	model            string
	thinkingLevel    pi.ThinkingLevel
	prompt           string
	piExecutable     string
	executionTimeout time.Duration
}

// workerHostRequestJSON is the wire shape of an execution request.
type workerHostRequestJSON struct {
	SchemaVersion    int    `json:"schemaVersion"`
	WorkerID         int    `json:"workerId"`
	Workspace        string `json:"workspace"`
	Model            string `json:"model"`
	ThinkingLevel    string `json:"thinkingLevel,omitempty"`
	Prompt           string `json:"prompt"`
	PiExecutable     string `json:"piExecutable"`
	ExecutionTimeout string `json:"executionTimeout"`
}

// encodeWorkerHostRequest validates req and returns its wire JSON.
func encodeWorkerHostRequest(req workerHostRequest) ([]byte, error) {
	if err := validateWorkerHostRequest(req); err != nil {
		return nil, fmt.Errorf("encode worker host request: %w", err)
	}
	data, err := json.Marshal(workerHostRequestJSON{
		SchemaVersion:    workerHostSchemaVersion,
		WorkerID:         req.workerID,
		Workspace:        req.workspace,
		Model:            req.model,
		ThinkingLevel:    string(req.thinkingLevel),
		Prompt:           req.prompt,
		PiExecutable:     req.piExecutable,
		ExecutionTimeout: req.executionTimeout.String(),
	})
	if err != nil {
		return nil, fmt.Errorf("encode worker host request: %w", err)
	}
	return data, nil
}

// decodeWorkerHostRequest parses exactly one strict JSON document —
// unknown fields and trailing data are rejected — and validates the
// reconstructed request. The input must be valid UTF-8.
func decodeWorkerHostRequest(data []byte) (workerHostRequest, error) {
	if !utf8.Valid(data) {
		return workerHostRequest{}, fmt.Errorf("decode worker host request: input is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var wire workerHostRequestJSON
	if err := dec.Decode(&wire); err != nil {
		return workerHostRequest{}, fmt.Errorf("decode worker host request: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return workerHostRequest{}, fmt.Errorf("decode worker host request: trailing data after document")
		}
		return workerHostRequest{}, fmt.Errorf("decode worker host request: %w", err)
	}
	if wire.SchemaVersion != workerHostSchemaVersion {
		return workerHostRequest{}, fmt.Errorf("decode worker host request: schemaVersion must be %d, got %d", workerHostSchemaVersion, wire.SchemaVersion)
	}
	req := workerHostRequest{
		workerID:      wire.WorkerID,
		workspace:     wire.Workspace,
		model:         wire.Model,
		thinkingLevel: pi.ThinkingLevel(wire.ThinkingLevel),
		prompt:        wire.Prompt,
		piExecutable:  wire.PiExecutable,
	}
	d, err := time.ParseDuration(wire.ExecutionTimeout)
	if err != nil {
		return workerHostRequest{}, fmt.Errorf("decode worker host request: executionTimeout is not a valid duration: %q", wire.ExecutionTimeout)
	}
	req.executionTimeout = d
	if err := validateWorkerHostRequest(req); err != nil {
		return workerHostRequest{}, fmt.Errorf("decode worker host request: %w", err)
	}
	return req, nil
}

// validateWorkerHostRequest is the single validator shared by encode and
// decode; it checks the domain struct, never the wire shape. Every rule
// mirrors a check the executed pi.Worker itself performs, so a request
// that passes here can still be rejected by the worker before Pi
// launches, while a request that fails here never reaches a host.
func validateWorkerHostRequest(req workerHostRequest) error {
	if req.workerID <= 0 {
		return fmt.Errorf("workerId must be positive, got %d", req.workerID)
	}
	if req.workspace == "" {
		return fmt.Errorf("workspace is required")
	}
	if req.model == "" {
		return fmt.Errorf("model is required")
	}
	provider, id, ok := strings.Cut(req.model, "/")
	if !ok {
		return fmt.Errorf("model must be provider/id with non-empty halves")
	}
	if _, ruleOK := pi.ExactModelSelector(provider, id); !ruleOK {
		return fmt.Errorf("model must be provider/id with non-empty halves")
	}
	if req.thinkingLevel != "" {
		if _, ok := pi.ParseThinkingLevel(string(req.thinkingLevel)); !ok {
			return fmt.Errorf("thinkingLevel is not a valid Pi thinking level")
		}
	}
	if !utf8.ValidString(req.prompt) {
		return fmt.Errorf("prompt is not valid UTF-8")
	}
	if strings.TrimSpace(req.prompt) == "" {
		return fmt.Errorf("prompt must not be blank")
	}
	if req.piExecutable == "" {
		return fmt.Errorf("piExecutable is required")
	}
	if req.executionTimeout <= 0 {
		return fmt.Errorf("executionTimeout must be positive, got %s", req.executionTimeout)
	}
	return nil
}

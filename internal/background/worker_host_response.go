package background

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/arasovic/pi-worker/internal/pi"
)

// workerHostResponseSchemaVersion is the only wire schema version
// accepted and produced for a worker-host response frame.
const workerHostResponseSchemaVersion = 1

// workerHostFrameKind names one worker-host response frame kind.
type workerHostFrameKind string

const (
	// workerHostFrameProcessStart is one process-start notification: the
	// child host reports the identity of one Pi process it launched —
	// including every startup-retry identity, in launch order, before the
	// terminal result frame.
	workerHostFrameProcessStart workerHostFrameKind = "process-start"
	// workerHostFrameResult is the single terminal result frame carrying
	// exactly one pi.WorkerResult, preserved field for field.
	workerHostFrameResult workerHostFrameKind = "result"
)

// workerHostResponse is one decoded worker-host response frame.
type workerHostResponse struct {
	kind     workerHostFrameKind
	workerID int // process-start frames only
	pid      int // process-start frames only
	result   pi.WorkerResult
}

// workerHostResponseJSON is the wire shape of one worker-host response
// frame. Result is an inline value so the payload key is always present
// on the wire for result frames; process-start frames carry only the two
// id fields.
type workerHostResponseJSON struct {
	SchemaVersion int             `json:"schemaVersion"`
	Kind          string          `json:"kind"`
	WorkerID      *int            `json:"workerId,omitempty"`
	PID           *int            `json:"pid,omitempty"`
	Result        json.RawMessage `json:"result,omitempty"`
}

// encodeWorkerHostProcessStart validates one process identity and returns
// its response frame as JSON bytes.
func encodeWorkerHostProcessStart(workerID, pid int) ([]byte, error) {
	if workerID <= 0 {
		return nil, fmt.Errorf("encode worker host response: workerId must be positive, got %d", workerID)
	}
	if pid <= 0 {
		return nil, fmt.Errorf("encode worker host response: pid must be positive, got %d", pid)
	}
	data, err := json.Marshal(workerHostResponseJSON{
		SchemaVersion: workerHostResponseSchemaVersion,
		Kind:          string(workerHostFrameProcessStart),
		WorkerID:      &workerID,
		PID:           &pid,
	})
	if err != nil {
		return nil, fmt.Errorf("encode worker host response: %w", err)
	}
	return data, nil
}

// encodeWorkerHostResult returns the terminal result frame for one
// pi.WorkerResult as JSON bytes. The result is preserved verbatim: its
// fields pass through unchanged, and nothing is added, removed, or
// validated here — the strict decode side is what refuses invalid
// terminal results, and an invalid terminal result must never imply
// success.
func encodeWorkerHostResult(result pi.WorkerResult) ([]byte, error) {
	resultData, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode worker host response: %w", err)
	}
	data, err := json.Marshal(workerHostResponseJSON{
		SchemaVersion: workerHostResponseSchemaVersion,
		Kind:          string(workerHostFrameResult),
		Result:        resultData,
	})
	if err != nil {
		return nil, fmt.Errorf("encode worker host response: %w", err)
	}
	return data, nil
}

// decodeWorkerHostResponse parses exactly one strict JSON document into a
// typed response frame. The input must be valid UTF-8; unknown fields,
// trailing data, a wrong schema version, an unknown kind, payload fields
// that do not belong to the kind, absent or null payload values, and an
// invalid terminal worker result are all rejected.
func decodeWorkerHostResponse(data []byte) (workerHostResponse, error) {
	if !utf8.Valid(data) {
		return workerHostResponse{}, fmt.Errorf("decode worker host response: input is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var wire workerHostResponseJSON
	if err := dec.Decode(&wire); err != nil {
		return workerHostResponse{}, fmt.Errorf("decode worker host response: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return workerHostResponse{}, fmt.Errorf("decode worker host response: trailing data after document")
		}
		return workerHostResponse{}, fmt.Errorf("decode worker host response: %w", err)
	}
	if wire.SchemaVersion != workerHostResponseSchemaVersion {
		return workerHostResponse{}, fmt.Errorf("decode worker host response: schemaVersion must be %d, got %d", workerHostResponseSchemaVersion, wire.SchemaVersion)
	}
	switch workerHostFrameKind(wire.Kind) {
	case workerHostFrameProcessStart:
		if wire.Result != nil {
			return workerHostResponse{}, fmt.Errorf("decode worker host response: process-start frame must not carry a result payload")
		}
		if wire.WorkerID == nil || wire.PID == nil {
			return workerHostResponse{}, fmt.Errorf("decode worker host response: process-start frame requires workerId and pid")
		}
		if *wire.WorkerID <= 0 {
			return workerHostResponse{}, fmt.Errorf("decode worker host response: process-start workerId must be positive, got %d", *wire.WorkerID)
		}
		if *wire.PID <= 0 {
			return workerHostResponse{}, fmt.Errorf("decode worker host response: process-start pid must be positive, got %d", *wire.PID)
		}
		return workerHostResponse{kind: workerHostFrameProcessStart, workerID: *wire.WorkerID, pid: *wire.PID}, nil
	case workerHostFrameResult:
		if wire.WorkerID != nil || wire.PID != nil {
			return workerHostResponse{}, fmt.Errorf("decode worker host response: result frame must not carry a process identity payload")
		}
		if len(wire.Result) == 0 || isJSONNull(wire.Result) {
			return workerHostResponse{}, fmt.Errorf("decode worker host response: result frame requires a result payload")
		}
		payloadDec := json.NewDecoder(bytes.NewReader(wire.Result))
		payloadDec.DisallowUnknownFields()
		var result pi.WorkerResult
		if err := payloadDec.Decode(&result); err != nil {
			return workerHostResponse{}, fmt.Errorf("decode worker host response: result: %w", err)
		}
		var resultExtra any
		if err := payloadDec.Decode(&resultExtra); err != io.EOF {
			if err == nil {
				return workerHostResponse{}, fmt.Errorf("decode worker host response: result: trailing data after document")
			}
			return workerHostResponse{}, fmt.Errorf("decode worker host response: result: %w", err)
		}
		if err := validateWorkerHostTerminalResult(result); err != nil {
			return workerHostResponse{}, fmt.Errorf("decode worker host response: %w", err)
		}
		return workerHostResponse{kind: workerHostFrameResult, result: result}, nil
	default:
		return workerHostResponse{}, fmt.Errorf("decode worker host response: unknown kind %q", wire.Kind)
	}
}

// validateWorkerHostTerminalResult checks the one property a terminal
// result must carry: a defined worker status. The worker itself always
// produces a defined status, so an absent or unknown status proves the
// terminal frame is invalid — and an invalid terminal result never
// implies success.
func validateWorkerHostTerminalResult(result pi.WorkerResult) error {
	switch result.Status {
	case pi.StatusCompleted, pi.StatusFailed, pi.StatusTimedOut,
		pi.StatusCancelled, pi.StatusUnavailable, pi.StatusError:
		return nil
	default:
		return fmt.Errorf("result status %q is not a defined worker status", result.Status)
	}
}

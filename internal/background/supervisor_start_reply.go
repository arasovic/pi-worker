package background

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// supervisorStartReplySchemaVersion is the only wire schema version
// accepted and produced for a supervisor start reply.
const supervisorStartReplySchemaVersion = 1

// supervisorStartReply is the strict answer to a start request: exactly
// one validated accepted Snapshot, or exactly one non-blank rejection
// message. The zero reply carries neither and is never produced.
type supervisorStartReply struct {
	snapshot Snapshot
	reason   string
}

// accepted reports whether the reply carries a snapshot. A reply carries
// a snapshot or a reason, never both and never neither.
func (r supervisorStartReply) accepted() bool { return r.reason == "" }

// supervisorStartReplyJSON is the wire shape of a start reply used for
// encoding. marshalSupervisorStartReply fills exactly one of Snapshot or
// Reason so omitempty keeps the other key off the wire. Decoding never
// uses this shape: pointer fields collapse an absent key and an explicit
// null into the same nil, so decode goes through
// supervisorStartReplyDecodeJSON and parses each payload from its raw
// bytes.
type supervisorStartReplyJSON struct {
	SchemaVersion int       `json:"schemaVersion"`
	Snapshot      *Snapshot `json:"accepted,omitempty"`
	Reason        *string   `json:"rejected,omitempty"`
}

// supervisorStartReplyDecodeJSON is the decode-only wire shape of a
// start reply. Accepted and Rejected are json.RawMessage fields so key
// presence survives decoding: an absent key leaves the field nil while
// an explicit null is the literal bytes "null". The exactly-one rule
// runs on that raw presence, and each payload value is parsed only
// afterwards.
type supervisorStartReplyDecodeJSON struct {
	SchemaVersion int             `json:"schemaVersion"`
	Accepted      json.RawMessage `json:"accepted"`
	Rejected      json.RawMessage `json:"rejected"`
}

// isJSONNull reports whether raw holds the JSON null literal.
func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(raw, []byte("null"))
}

// encodeSupervisorStartAccepted validates snap, requires it to be in the
// accepted state, and returns its reply as JSON bytes. Validate already
// refuses a terminal accepted snapshot, so no separate terminal check is
// needed here.
func encodeSupervisorStartAccepted(snap Snapshot) ([]byte, error) {
	if err := snap.Validate(); err != nil {
		return nil, fmt.Errorf("encode supervisor start reply: validate snapshot: %w", err)
	}
	if snap.State != RunAccepted {
		return nil, fmt.Errorf("encode supervisor start reply: snapshot state must be %q, got %q", RunAccepted, snap.State)
	}
	return marshalSupervisorStartReply(supervisorStartReply{snapshot: snap})
}

// encodeSupervisorStartRejected validates reason and returns a rejection
// reply as JSON bytes.
func encodeSupervisorStartRejected(reason string) ([]byte, error) {
	if !utf8.ValidString(reason) {
		return nil, fmt.Errorf("encode supervisor start reply: reason is not valid UTF-8")
	}
	if strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("encode supervisor start reply: reason must not be blank")
	}
	return marshalSupervisorStartReply(supervisorStartReply{reason: reason})
}

// marshalSupervisorStartReply serializes one reply to its wire shape.
func marshalSupervisorStartReply(reply supervisorStartReply) ([]byte, error) {
	var wire supervisorStartReplyJSON
	wire.SchemaVersion = supervisorStartReplySchemaVersion
	if reply.accepted() {
		snap := reply.snapshot
		wire.Snapshot = &snap
	} else {
		reason := reply.reason
		wire.Reason = &reason
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("encode supervisor start reply: %w", err)
	}
	return data, nil
}

// decodeSupervisorStartReply parses exactly one strict JSON document into
// a private reply. The input must be valid UTF-8; unknown fields, trailing
// data, a wrong schema version, both or neither payload key, an explicit
// null payload, and any invalid, terminal, or non-accepted Snapshot are
// rejected. Accepted and rejected are read as raw values first, so the
// exactly-one rule is judged on key presence: an explicit null is present
// but never a usable payload.
func decodeSupervisorStartReply(data []byte) (supervisorStartReply, error) {
	if !utf8.Valid(data) {
		return supervisorStartReply{}, fmt.Errorf("decode supervisor start reply: input is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var wire supervisorStartReplyDecodeJSON
	if err := dec.Decode(&wire); err != nil {
		return supervisorStartReply{}, fmt.Errorf("decode supervisor start reply: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return supervisorStartReply{}, fmt.Errorf("decode supervisor start reply: trailing data after document")
		}
		return supervisorStartReply{}, fmt.Errorf("decode supervisor start reply: %w", err)
	}
	if wire.SchemaVersion != supervisorStartReplySchemaVersion {
		return supervisorStartReply{}, fmt.Errorf("decode supervisor start reply: schemaVersion must be %d, got %d", supervisorStartReplySchemaVersion, wire.SchemaVersion)
	}
	switch {
	case wire.Accepted != nil && wire.Rejected != nil:
		return supervisorStartReply{}, fmt.Errorf("decode supervisor start reply: accepted and rejected are mutually exclusive")
	case wire.Accepted == nil && wire.Rejected == nil:
		return supervisorStartReply{}, fmt.Errorf("decode supervisor start reply: exactly one of accepted or rejected is required")
	case wire.Accepted != nil:
		if isJSONNull(wire.Accepted) {
			return supervisorStartReply{}, fmt.Errorf("decode supervisor start reply: accepted must not be null")
		}
		payloadDec := json.NewDecoder(bytes.NewReader(wire.Accepted))
		payloadDec.DisallowUnknownFields()
		var snap Snapshot
		if err := payloadDec.Decode(&snap); err != nil {
			return supervisorStartReply{}, fmt.Errorf("decode supervisor start reply: accepted: %w", err)
		}
		if err := snap.Validate(); err != nil {
			return supervisorStartReply{}, fmt.Errorf("decode supervisor start reply: accepted snapshot is invalid: %w", err)
		}
		if snap.State != RunAccepted {
			return supervisorStartReply{}, fmt.Errorf("decode supervisor start reply: snapshot state must be %q, got %q", RunAccepted, snap.State)
		}
		return supervisorStartReply{snapshot: snap}, nil
	default:
		if isJSONNull(wire.Rejected) {
			return supervisorStartReply{}, fmt.Errorf("decode supervisor start reply: rejected must not be null")
		}
		var reason string
		if err := json.Unmarshal(wire.Rejected, &reason); err != nil {
			return supervisorStartReply{}, fmt.Errorf("decode supervisor start reply: rejected: %w", err)
		}
		if !utf8.ValidString(reason) {
			return supervisorStartReply{}, fmt.Errorf("decode supervisor start reply: rejected reason is not valid UTF-8")
		}
		if strings.TrimSpace(reason) == "" {
			return supervisorStartReply{}, fmt.Errorf("decode supervisor start reply: rejected reason must not be blank")
		}
		return supervisorStartReply{reason: reason}, nil
	}
}

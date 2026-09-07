package background

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/run"
)

// replyFixtureTime is the fixed wall-clock used by every reply fixture so
// that ValidateRunID never fights real clock drift.
var replyFixtureTime = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// replyFixtureRunID is a run ID whose prefix matches replyFixtureTime.
const replyFixtureRunID = "20260901T120000Z-4242"

// acceptedSnapshotFixture returns one valid, accepted, non-terminal
// Snapshot built as a plain struct value: exactly one queued worker with
// one declared write and one data file. Each call returns fresh memory so
// tests may mutate freely.
func acceptedSnapshotFixture() Snapshot {
	now := replyFixtureTime
	return Snapshot{
		SchemaVersion: 1,
		RunID:         replyFixtureRunID,
		State:         RunAccepted,
		Terminal:      false,
		AcceptedAt:    now,
		UpdatedAt:     now,
		Workspace:     "/ws",
		Supervisor:    ProcessIdentity{PID: 1, CreateTime: 100},
		Workers: []WorkerSnapshot{
			{
				WorkerID:         1,
				State:            WorkerQueued,
				AcceptedAt:       now,
				QueueDeadline:    now.Add(15 * time.Minute),
				ExecutionTimeout: "1m0s",
				Task: run.TaskProjection{
					Model:          "fake/model",
					ThinkingLevel:  string(pi.ThinkingLow),
					Prompt:         "do the thing",
					WritesDeclared: true,
					Writes:         []string{"a.txt"},
					Data:           []pi.DataFile{{Path: "readme.md", Bytes: 11, SHA256: "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"}},
				},
			},
		},
	}
}

// runningSnapshotFixture returns acceptedSnapshotFixture promoted to
// the running state in a way that satisfies Snapshot.Validate on its
// own: one worker running with a startedAt and a process identity.
// Each call returns fresh memory so tests may mutate freely.
func runningSnapshotFixture() Snapshot {
	snap := acceptedSnapshotFixture()
	snap.State = RunRunning
	started := snap.AcceptedAt.Add(time.Second)
	w := snap.Workers[0]
	w.State = WorkerRunning
	w.StartedAt = &started
	w.Process = &ProcessIdentity{PID: 7, CreateTime: 200}
	snap.Workers = []WorkerSnapshot{w}
	return snap
}

// acceptedReplyJSON pins the exact embedded snapshot shape of
// acceptedSnapshotFixture as a single-quoted Go string.
const acceptedReplyJSON = `{"schemaVersion":1,"accepted":{"schemaVersion":1,"runId":"20260901T120000Z-4242","state":"accepted","terminal":false,"acceptedAt":"2026-09-01T12:00:00Z","updatedAt":"2026-09-01T12:00:00Z","workspace":"/ws","supervisor":{"pid":1,"createTime":100},"workers":[{"workerId":1,"state":"queued","acceptedAt":"2026-09-01T12:00:00Z","queueDeadline":"2026-09-01T12:15:00Z","executionTimeout":"1m0s","task":{"model":"fake/model","thinkingLevel":"low","prompt":"do the thing","promptTruncated":false,"writesDeclared":true,"writes":["a.txt"],"data":[{"path":"readme.md","byteCount":11,"sha256":"b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"}]}}]}}`

// TestSupervisorStartReplyEncodeAcceptedExactJSON pins the exact wire
// shape of an accepted reply: schemaVersion 1, one accepted field whose
// value is the full snapshot document, and no rejected field.
func TestSupervisorStartReplyEncodeAcceptedExactJSON(t *testing.T) {
	encoded, err := encodeSupervisorStartAccepted(acceptedSnapshotFixture())
	if err != nil {
		t.Fatalf("encode accepted: %v", err)
	}
	if string(encoded) != acceptedReplyJSON {
		t.Fatalf("encoded JSON mismatch:\n got: %s\nwant: %s", encoded, acceptedReplyJSON)
	}
}

// TestSupervisorStartReplyEncodeRejectedExactJSON pins the exact wire
// shape of a rejected reply: schemaVersion 1, one rejected field carrying
// the message, and no accepted field.
func TestSupervisorStartReplyEncodeRejectedExactJSON(t *testing.T) {
	encoded, err := encodeSupervisorStartRejected("boom")
	if err != nil {
		t.Fatalf("encode rejected: %v", err)
	}
	want := `{"schemaVersion":1,"rejected":"boom"}`
	if string(encoded) != want {
		t.Fatalf("encoded JSON mismatch:\n got: %s\nwant: %s", encoded, want)
	}
}

// TestSupervisorStartReplyAcceptedRoundTrip encodes an accepted reply and
// decodes it back: the reply reports acceptance and the snapshot survives
// whole, including nested slices. Re-encoding the decoded reply
// reproduces the original bytes.
func TestSupervisorStartReplyAcceptedRoundTrip(t *testing.T) {
	snap := acceptedSnapshotFixture()
	encoded, err := encodeSupervisorStartAccepted(snap)
	if err != nil {
		t.Fatalf("encode accepted: %v", err)
	}
	reply, err := decodeSupervisorStartReply(encoded)
	if err != nil {
		t.Fatalf("decode accepted: %v", err)
	}
	if !reply.accepted() {
		t.Fatalf("reply reports rejection with reason %q", reply.reason)
	}
	if got := reply.snapshot; !reflect.DeepEqual(got, snap) {
		t.Fatalf("decoded snapshot mismatch:\n got: %+v\nwant: %+v", got, snap)
	}

	reencoded, err := encodeSupervisorStartAccepted(reply.snapshot)
	if err != nil {
		t.Fatalf("re-encode accepted: %v", err)
	}
	if !bytes.Equal(encoded, reencoded) {
		t.Fatalf("re-encoded bytes differ:\n got: %s\nwant: %s", reencoded, encoded)
	}
}

// TestSupervisorStartReplyRejectedRoundTrip encodes a rejection and
// decodes it back: the reply reports rejection with the same message, and
// re-encoding reproduces the original bytes.
func TestSupervisorStartReplyRejectedRoundTrip(t *testing.T) {
	encoded, err := encodeSupervisorStartRejected("worker host unavailable")
	if err != nil {
		t.Fatalf("encode rejected: %v", err)
	}
	reply, err := decodeSupervisorStartReply(encoded)
	if err != nil {
		t.Fatalf("decode rejected: %v", err)
	}
	if reply.accepted() {
		t.Fatal("reply reports acceptance for a rejection")
	}
	if reply.reason != "worker host unavailable" {
		t.Fatalf("reason mismatch: %q", reply.reason)
	}

	reencoded, err := encodeSupervisorStartRejected(reply.reason)
	if err != nil {
		t.Fatalf("re-encode rejected: %v", err)
	}
	if !bytes.Equal(encoded, reencoded) {
		t.Fatalf("re-encoded bytes differ:\n got: %s\nwant: %s", reencoded, encoded)
	}
}

// buildReplyDocument marshals v, used for every decode-side fixture so
// tests never resort to string surgery.
func buildReplyDocument(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal reply wire: %v", err)
	}
	return data
}

// replyWireWithAccepted returns a valid accepted wire document, or nil if
// building the embedded snapshot fails.
func replyWireWithAccepted(t *testing.T) []byte {
	t.Helper()
	snap := acceptedSnapshotFixture()
	return buildReplyDocument(t, supervisorStartReplyJSON{
		SchemaVersion: supervisorStartReplySchemaVersion,
		Snapshot:      &snap,
	})
}

// TestSupervisorStartReplyDecodeRejects covers the decode-side failures
// that are not snapshot-payload or one-of failures: unknown fields,
// trailing JSON, invalid UTF-8, and a wrong schema version.
func TestSupervisorStartReplyDecodeRejects(t *testing.T) {
	valid := replyWireWithAccepted(t)

	// Build the unknown-field document structurally.
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(valid, &doc); err != nil {
		t.Fatalf("unmarshal valid wire: %v", err)
	}
	doc["surprise"] = json.RawMessage("true")
	withUnknown := buildReplyDocument(t, doc)

	trailing := append(append([]byte(nil), valid...), ' ', '{', '}')

	tests := []struct {
		name    string
		data    []byte
		wantErr string
	}{
		{"unknown field", withUnknown, "unknown field"},
		{"trailing document", trailing, "trailing data"},
		{"invalid utf-8", []byte("{\"rejected\":\"\xff\"}"), "not valid UTF-8"},
		{"wrong version", buildReplyDocument(t, supervisorStartReplyJSON{SchemaVersion: supervisorStartReplySchemaVersion + 1}), "schemaVersion must be 1, got 2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeSupervisorStartReply(tt.data)
			if err == nil {
				t.Fatalf("decode succeeded, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestSupervisorStartReplyDecodeOneOfRule covers the exactly-one-of rule:
// neither payload field and both payload fields are rejected, on the
// decode side and, where encode builds the document, on the encode side
// through its own validation. Null payloads next to the other payload
// are covered by TestSupervisorStartReplyDecodeRejectsNullPayloads.
func TestSupervisorStartReplyDecodeOneOfRule(t *testing.T) {
	accepted := replyWireWithAccepted(t)
	rejected := buildReplyDocument(t, supervisorStartReplyJSON{
		SchemaVersion: supervisorStartReplySchemaVersion,
		Reason:        ptrString("boom"),
	})

	// Neither field: an empty object with only the version.
	neither := buildReplyDocument(t, supervisorStartReplyJSON{
		SchemaVersion: supervisorStartReplySchemaVersion,
	})

	// Both fields: inject a rejected field into the accepted document
	// structurally so the combined document stays valid JSON.
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(accepted, &doc); err != nil {
		t.Fatalf("unmarshal accepted wire: %v", err)
	}
	var rejectedDoc map[string]json.RawMessage
	if err := json.Unmarshal(rejected, &rejectedDoc); err != nil {
		t.Fatalf("unmarshal rejected wire: %v", err)
	}
	doc["rejected"] = rejectedDoc["rejected"]
	both := buildReplyDocument(t, doc)

	tests := []struct {
		name    string
		data    []byte
		wantErr string
	}{
		{"neither field", neither, "exactly one of accepted or rejected is required"},
		{"both fields", both, "mutually exclusive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeSupervisorStartReply(tt.data)
			if err == nil {
				t.Fatalf("decode succeeded, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestSupervisorStartReplyDecodeRejectsNullPayloads covers explicit null
// payloads. A null is a present key without a usable value: a sole null
// is refused, and a null next to the other payload still trips the
// mutually-exclusive rule, which is judged on raw key presence rather
// than on decoded values.
func TestSupervisorStartReplyDecodeRejectsNullPayloads(t *testing.T) {
	// Valid accepted document with an explicit null rejected field
	// injected structurally so the combined document stays valid JSON.
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(replyWireWithAccepted(t), &doc); err != nil {
		t.Fatalf("unmarshal accepted wire: %v", err)
	}
	doc["rejected"] = json.RawMessage("null")
	acceptedWithNullRejected := buildReplyDocument(t, doc)

	tests := []struct {
		name    string
		data    []byte
		wantErr string
	}{
		{
			"accepted null alone",
			[]byte(`{"schemaVersion":1,"accepted":null}`),
			"accepted must not be null",
		},
		{
			"rejected null alone",
			[]byte(`{"schemaVersion":1,"rejected":null}`),
			"rejected must not be null",
		},
		{
			"accepted null plus valid rejected",
			[]byte(`{"schemaVersion":1,"accepted":null,"rejected":"boom"}`),
			"mutually exclusive",
		},
		{
			"valid accepted plus rejected null",
			acceptedWithNullRejected,
			"mutually exclusive",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeSupervisorStartReply(tt.data)
			if err == nil {
				t.Fatalf("decode succeeded, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestSupervisorStartReplyEncodeRejectsBlankRejection covers the encode
// side of the rejection contract: the message must be valid UTF-8 and
// non-blank. Whitespace-only counts as blank.
func TestSupervisorStartReplyEncodeRejectsBlankRejection(t *testing.T) {
	tests := []struct {
		name   string
		reason string
	}{
		{"empty", ""},
		{"whitespace only", " \t\n "},
		{"invalid utf-8", "bad\xff"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := encodeSupervisorStartRejected(tt.reason)
			if err == nil {
				t.Fatalf("encode rejected %q succeeded, want error", tt.reason)
			}
		})
	}
}

// TestSupervisorStartReplyDecodeRejectsBlankRejection covers the decode
// side of the rejection contract: blank messages are refused. Explicit
// null payloads are covered by
// TestSupervisorStartReplyDecodeRejectsNullPayloads.
func TestSupervisorStartReplyDecodeRejectsBlankRejection(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		wantErr string
	}{
		{
			"empty message",
			[]byte(`{"schemaVersion":1,"rejected":""}`),
			"rejected reason must not be blank",
		},
		{
			"whitespace message",
			[]byte(`{"schemaVersion":1,"rejected":" \t "}`),
			"rejected reason must not be blank",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeSupervisorStartReply(tt.data)
			if err == nil {
				t.Fatalf("decode succeeded, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

// replyWireWithInvalidSnapshot returns an accepted wire document whose
// embedded snapshot fails Snapshot.Validate after the specific mutation.
func replyWireWithInvalidSnapshot(t *testing.T, mutate func(*Snapshot)) []byte {
	t.Helper()
	snap := acceptedSnapshotFixture()
	mutate(&snap)
	return buildReplyDocument(t, supervisorStartReplyJSON{
		SchemaVersion: supervisorStartReplySchemaVersion,
		Snapshot:      &snap,
	})
}

// TestSupervisorStartReplyEncodeSnapshotValidation covers the encode
// side: the snapshot must pass Snapshot.Validate and additionally be
// accepted and non-terminal.
func TestSupervisorStartReplyEncodeSnapshotValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Snapshot)
		wantErr string
	}{
		{"invalid snapshot", func(s *Snapshot) { s.Workspace = "" }, "workspace must not be empty"},
		{"terminal snapshot", func(s *Snapshot) { s.Terminal = true }, "accepted: terminal must be false"},
		{"non-accepted state", func(s *Snapshot) { *s = runningSnapshotFixture() }, `state must be "accepted", got "running"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := acceptedSnapshotFixture()
			tt.mutate(&snap)
			_, err := encodeSupervisorStartAccepted(snap)
			if err == nil {
				t.Fatalf("encode accepted succeeded, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestSupervisorStartReplyDecodeRejectsBadAcceptedSnapshot covers the
// decode side: an embedded snapshot that fails Snapshot.Validate, is
// terminal, or is not in the accepted state is refused. The running
// fixture also satisfies Snapshot.Validate on its own, proving the reply
// layer adds the accepted-state rule on top of plain validity.
func TestSupervisorStartReplyDecodeRejectsBadAcceptedSnapshot(t *testing.T) {
	// A running snapshot that satisfies Snapshot.Validate on its own.
	runningWire := func() []byte {
		snap := runningSnapshotFixture()
		if err := snap.Validate(); err != nil {
			t.Fatalf("running fixture must satisfy Snapshot.Validate: %v", err)
		}
		return buildReplyDocument(t, supervisorStartReplyJSON{
			SchemaVersion: supervisorStartReplySchemaVersion,
			Snapshot:      &snap,
		})
	}()

	tests := []struct {
		name    string
		data    []byte
		wantErr string
	}{
		{"invalid snapshot", replyWireWithInvalidSnapshot(t, func(s *Snapshot) { s.Workspace = "" }), "accepted snapshot is invalid: workspace must not be empty"},
		{"terminal snapshot", replyWireWithInvalidSnapshot(t, func(s *Snapshot) { s.Terminal = true }), "accepted snapshot is invalid: accepted: terminal must be false"},
		{"non-accepted state", runningWire, `snapshot state must be "accepted", got "running"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeSupervisorStartReply(tt.data)
			if err == nil {
				t.Fatalf("decode succeeded, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestSupervisorStartReplyDecodedSnapshotDoesNotAliasInput checks that the
// decoded accepted snapshot owns its memory: mutating the input bytes
// afterwards cannot change the snapshot or its nested slices.
func TestSupervisorStartReplyDecodedSnapshotDoesNotAliasInput(t *testing.T) {
	encoded := replyWireWithAccepted(t)

	reply, err := decodeSupervisorStartReply(encoded)
	if err != nil {
		t.Fatalf("decode accepted: %v", err)
	}

	// Corrupt every input byte.
	for i := range encoded {
		encoded[i] = ' '
	}

	if err := reply.snapshot.Validate(); err != nil {
		t.Fatalf("decoded snapshot became invalid after input mutation: %v", err)
	}
	if reply.snapshot.Workers[0].Task.Writes[0] != "a.txt" {
		t.Fatalf("decoded writes aliased input: %q", reply.snapshot.Workers[0].Task.Writes[0])
	}
	if reply.snapshot.Workers[0].Task.Data[0].Path != "readme.md" {
		t.Fatalf("decoded data aliased input: %q", reply.snapshot.Workers[0].Task.Data[0].Path)
	}
	if reply.snapshot.RunID != replyFixtureRunID {
		t.Fatalf("decoded runId aliased input: %q", reply.snapshot.RunID)
	}
}

// TestSupervisorStartReplyEncodeRoundTripsThroughJSONRoundTripHelper
// verifies the struct fixture decodes through a byte-level JSON round trip
// (marshal the wire struct, unmarshal it back, compare with reflect):
// this guards the fixture against silent field-tag drift.
func TestSupervisorStartReplyWireJSONRoundTripHelper(t *testing.T) {
	snap := acceptedSnapshotFixture()
	wire := supervisorStartReplyJSON{
		SchemaVersion: supervisorStartReplySchemaVersion,
		Snapshot:      &snap,
	}
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal wire: %v", err)
	}
	var back supervisorStartReplyJSON
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal wire: %v", err)
	}
	if back.Snapshot == nil || !reflect.DeepEqual(*back.Snapshot, snap) {
		t.Fatalf("snapshot lost in wire round trip")
	}
	if back.Reason != nil {
		t.Fatalf("unexpected reason in wire round trip: %q", *back.Reason)
	}

	reason := "boom"
	wire = supervisorStartReplyJSON{
		SchemaVersion: supervisorStartReplySchemaVersion,
		Reason:        &reason,
	}
	data, err = json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal wire: %v", err)
	}
	back = supervisorStartReplyJSON{}
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal wire: %v", err)
	}
	if back.Snapshot != nil {
		t.Fatal("unexpected snapshot in rejected wire round trip")
	}
	if back.Reason == nil || *back.Reason != "boom" {
		t.Fatalf("reason lost in wire round trip: %v", back.Reason)
	}
}

// TestSupervisorStartReplyDecodeExactBytes pins decode against the exact
// literal documents for both payload kinds, independent of the wire-struct
// fixtures.
func TestSupervisorStartReplyDecodeExactBytes(t *testing.T) {
	t.Run("accepted literal", func(t *testing.T) {
		reply, err := decodeSupervisorStartReply([]byte(acceptedReplyJSON))
		if err != nil {
			t.Fatalf("decode accepted literal: %v", err)
		}
		if !reply.accepted() {
			t.Fatalf("literal accepted document decoded as rejection")
		}
		if err := reply.snapshot.Validate(); err != nil {
			t.Fatalf("decoded literal snapshot invalid: %v", err)
		}
	})
	t.Run("rejected literal", func(t *testing.T) {
		reply, err := decodeSupervisorStartReply([]byte(`{"schemaVersion":1,"rejected":"no"}`))
		if err != nil {
			t.Fatalf("decode rejected literal: %v", err)
		}
		if reply.accepted() || reply.reason != "no" {
			t.Fatalf("literal rejected document decoded wrong: %+v", reply)
		}
	})
}

// ptrString is a local helper; ptrTime/ptrRunStatus live in the snapshot
// tests.
func ptrString(s string) *string { return &s }

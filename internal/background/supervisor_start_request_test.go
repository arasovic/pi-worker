package background

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/run"
	"github.com/arasovic/pi-worker/internal/worktree"
)

// validStartRequest returns one fully populated, valid start request: two
// ordered tasks, declared and declared-empty writes, ordered binary data,
// a verify argv, a prepared worktree, and every scalar filled in. Each
// call returns fresh memory so tests may mutate freely.
func validStartRequest() supervisorStartRequest {
	return supervisorStartRequest{
		runID:      "20250102T101112Z-4242",
		acceptedAt: time.Date(2025, 1, 2, 10, 11, 12, 0, time.UTC),
		workspace:  "ws",
		tasks: []run.Task{
			{
				Prompt:        "first task",
				Model:         "anthropic/claude-4",
				ThinkingLevel: pi.ThinkingMedium,
				Writes:        run.WriteDeclaration{Declared: true, Paths: []string{"a.txt"}},
				Data: []run.DataFile{
					{Path: "in/one.bin", Content: []byte{0x00, 0x01, 0xff, 0xfe}},
					{Path: "in/two.bin", Content: []byte{0xde, 0xad, 0xbe, 0xef}},
				},
			},
			{
				Prompt: "second task",
				Model:  "openai/gpt-5",
				// Declared with the empty set: this task writes nothing,
				// while task one keeps the run's declaration uniform.
				Writes: run.WriteDeclaration{Declared: true},
			},
		},
		verify:           []string{"go", "test", "./..."},
		executionTimeout: 90 * time.Minute,
		backgroundRoot:   "/bg",
		admissionRoot:    "/adm",
		maxModelWorkers:  2,
		worktree:         &worktree.Prepared{Name: "issue-204", Path: "/wt/issue-204", Branch: "run/issue-204", Head: "cafe1234"},
		piExecutable:     "/usr/bin/pi",
		debug:            true,
	}
}

// minimalStartRequest strips a valid request down to the smallest shape
// that still encodes: no verify, no worktree, no writes, no data, debug off.
func minimalStartRequest() supervisorStartRequest {
	req := validStartRequest()
	req.tasks = []run.Task{{Prompt: "p", Model: "anthropic/claude-4"}}
	req.verify = nil
	req.executionTimeout = time.Hour
	req.worktree = nil
	req.debug = false
	return req
}

// minimalStartRequestJSON is the wire shape of minimalStartRequest, built
// as a struct so decode-failure fixtures never resort to string surgery.
func minimalStartRequestJSON() supervisorStartRequestJSON {
	return supervisorStartRequestJSON{
		SchemaVersion:    supervisorStartSchemaVersion,
		RunID:            "20250102T101112Z-4242",
		AcceptedAt:       time.Date(2025, 1, 2, 10, 11, 12, 0, time.UTC),
		Workspace:        "ws",
		Tasks:            []taskJSON{{Prompt: "p", Model: "anthropic/claude-4"}},
		ExecutionTimeout: "1h0m0s",
		BackgroundRoot:   "/bg",
		AdmissionRoot:    "/adm",
		MaxModelWorkers:  2,
		PiExecutable:     "/usr/bin/pi",
	}
}

// bigUnicodePrompt returns a prompt well over 4 KB of multi-byte text.
func bigUnicodePrompt() string {
	return strings.Repeat("π→世界·✓ audit the run\n", 300)
}

func mustMarshalJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return data
}

// TestSupervisorStartRequestEncodeMinimalExactJSON pins the exact wire
// shape of the smallest valid request, including the duration carried as
// a string.
func TestSupervisorStartRequestEncodeMinimalExactJSON(t *testing.T) {
	encoded, err := encodeSupervisorStartRequest(minimalStartRequest())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := `{"schemaVersion":1,"runId":"20250102T101112Z-4242","acceptedAt":"2025-01-02T10:11:12Z","workspace":"ws","tasks":[{"prompt":"p","model":"anthropic/claude-4","writesDeclared":false}],"executionTimeout":"1h0m0s","backgroundRoot":"/bg","admissionRoot":"/adm","maxModelWorkers":2,"piExecutable":"/usr/bin/pi","debug":false}`
	if string(encoded) != want {
		t.Fatalf("encoded JSON mismatch:\n got: %s\nwant: %s", encoded, want)
	}
}

// TestSupervisorStartRequestRoundTrip encodes a maximal request and decodes
// it back, checking every field survives: a >4 KB Unicode prompt, two
// ordered tasks, declared versus declared-empty writes, ordered binary
// data, the verify argv, the optional worktree, roots, limit, and debug.
func TestSupervisorStartRequestRoundTrip(t *testing.T) {
	req := validStartRequest()
	big := bigUnicodePrompt()
	if len(big) <= 4096 {
		t.Fatalf("test prompt too small: %d bytes", len(big))
	}
	req.tasks[0].Prompt = big

	encoded, err := encodeSupervisorStartRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	back, err := decodeSupervisorStartRequest(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if back.runID != req.runID || back.workspace != req.workspace {
		t.Fatalf("identity mismatch: runID=%q workspace=%q", back.runID, back.workspace)
	}
	if !back.acceptedAt.Equal(req.acceptedAt) || back.acceptedAt.Location() != time.UTC {
		t.Fatalf("acceptedAt mismatch: %v", back.acceptedAt)
	}
	if back.executionTimeout != req.executionTimeout {
		t.Fatalf("executionTimeout mismatch: %s", back.executionTimeout)
	}
	if back.backgroundRoot != req.backgroundRoot || back.admissionRoot != req.admissionRoot {
		t.Fatalf("roots mismatch: %q %q", back.backgroundRoot, back.admissionRoot)
	}
	if back.maxModelWorkers != req.maxModelWorkers || back.piExecutable != req.piExecutable || back.debug != req.debug {
		t.Fatalf("scalars mismatch: %+v", back)
	}
	if !slices.Equal(back.verify, []string{"go", "test", "./..."}) {
		t.Fatalf("verify argv mismatch: %q", back.verify)
	}

	// Task order and content survive, including the big prompt.
	if len(back.tasks) != 2 {
		t.Fatalf("task count mismatch: %d", len(back.tasks))
	}
	first, second := back.tasks[0], back.tasks[1]
	if first.Prompt != big || second.Prompt != "second task" {
		t.Fatalf("task prompts not preserved in order")
	}
	if first.Model != "anthropic/claude-4" || first.ThinkingLevel != pi.ThinkingMedium {
		t.Fatalf("task 1 model/thinking mismatch: %q %q", first.Model, first.ThinkingLevel)
	}
	if second.Model != "openai/gpt-5" || second.ThinkingLevel != "" {
		t.Fatalf("task 2 model/thinking mismatch: %q %q", second.Model, second.ThinkingLevel)
	}

	// Declared writes with paths on task one, declared-empty on task two.
	if !first.Writes.Declared || !slices.Equal(first.Writes.Paths, []string{"a.txt"}) {
		t.Fatalf("task 1 writes mismatch: %+v", first.Writes)
	}
	if !second.Writes.Declared || len(second.Writes.Paths) != 0 {
		t.Fatalf("task 2 writes mismatch: %+v", second.Writes)
	}

	// Ordered binary data round trips byte for byte.
	if len(first.Data) != 2 {
		t.Fatalf("task 1 data count mismatch: %d", len(first.Data))
	}
	if first.Data[0].Path != "in/one.bin" || !bytes.Equal(first.Data[0].Content, []byte{0x00, 0x01, 0xff, 0xfe}) {
		t.Fatalf("task 1 data[0] mismatch: %+v", first.Data[0])
	}
	if first.Data[1].Path != "in/two.bin" || !bytes.Equal(first.Data[1].Content, []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Fatalf("task 1 data[1] mismatch: %+v", first.Data[1])
	}

	// The optional worktree survives.
	if back.worktree == nil {
		t.Fatalf("worktree lost in round trip")
	}
	if *back.worktree != (worktree.Prepared{Name: "issue-204", Path: "/wt/issue-204", Branch: "run/issue-204", Head: "cafe1234"}) {
		t.Fatalf("worktree mismatch: %+v", *back.worktree)
	}

	// Re-encoding the decoded request reproduces the same bytes.
	reencoded, err := encodeSupervisorStartRequest(back)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if !bytes.Equal(encoded, reencoded) {
		t.Fatalf("re-encoded bytes differ:\n got: %s\nwant: %s", reencoded, encoded)
	}
}

// TestSupervisorStartRequestDecodeRejects covers the decode-side failures:
// unknown fields, trailing JSON, invalid UTF-8, a wrong schema version,
// and malformed base64 in data content.
func TestSupervisorStartRequestDecodeRejects(t *testing.T) {
	valid := mustMarshalJSON(t, minimalStartRequestJSON())

	// Build the unknown-field document structurally, not by string surgery.
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(valid, &doc); err != nil {
		t.Fatalf("unmarshal valid wire: %v", err)
	}
	doc["surprise"] = json.RawMessage("true")
	withUnknown := mustMarshalJSON(t, doc)

	wrongVersion := minimalStartRequestJSON()
	wrongVersion.SchemaVersion = supervisorStartSchemaVersion + 1

	trailing := append(append([]byte(nil), valid...), ' ', '{', '}')

	// The content value is invalid base64; decoding fails on it before
	// any domain validation runs.
	malformedBase64 := []byte(`{"schemaVersion":1,"tasks":[{"prompt":"p","model":"anthropic/claude-4","writesDeclared":false,"data":[{"path":"f.bin","content":"@@@"}]}]}`)

	tests := []struct {
		name    string
		data    []byte
		wantErr string
	}{
		{"unknown field", withUnknown, "unknown field"},
		{"trailing document", trailing, "trailing data"},
		{"invalid utf-8", []byte("{\"workspace\":\"\xff\"}"), "not valid UTF-8"},
		{"wrong version", mustMarshalJSON(t, wrongVersion), "schemaVersion must be 1, got 2"},
		{"malformed base64", malformedBase64, "illegal base64"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeSupervisorStartRequest(tt.data)
			if err == nil {
				t.Fatalf("decode succeeded, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestSupervisorStartRequestEncodeValidation runs the shared validator
// through encode: every field group has a rejecting case.
func TestSupervisorStartRequestEncodeValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*supervisorStartRequest)
		wantErr string
	}{
		{"run id", func(r *supervisorStartRequest) { r.runID = "bogus" }, "runId"},
		{"zero acceptedAt", func(r *supervisorStartRequest) { r.acceptedAt = time.Time{} }, "acceptedAt is required"},
		{"non-UTC acceptedAt", func(r *supervisorStartRequest) {
			r.acceptedAt = r.acceptedAt.In(time.FixedZone("CET", 3600))
		}, "acceptedAt must be in UTC"},
		{"empty workspace", func(r *supervisorStartRequest) { r.workspace = "" }, "workspace is required"},
		{"no tasks", func(r *supervisorStartRequest) { r.tasks = nil }, "at least one task is required"},
		{"too many tasks", func(r *supervisorStartRequest) {
			tasks := make([]run.Task, run.MaxTasks+1)
			for i := range tasks {
				tasks[i] = run.Task{Prompt: "p", Model: "anthropic/claude-4"}
			}
			r.tasks = tasks
		}, "at most 3 tasks are supported"},
		{"empty model", func(r *supervisorStartRequest) { r.tasks[0].Model = "" }, "model is required"},
		{"malformed model", func(r *supervisorStartRequest) { r.tasks[0].Model = "claude" }, "provider/id"},
		{"invalid thinking level", func(r *supervisorStartRequest) { r.tasks[0].ThinkingLevel = pi.ThinkingLevel("bogus") }, "thinkingLevel"},
		{"blank prompt", func(r *supervisorStartRequest) { r.tasks[0].Prompt = " \t\n " }, "prompt must not be blank"},
		{"write paths without declaration", func(r *supervisorStartRequest) {
			r.tasks[0].Writes.Declared = false
		}, "write paths require writesDeclared"},
		{"mixed declaration", func(r *supervisorStartRequest) {
			r.tasks[1].Writes.Declared = false
		}, "all-or-none"},
		{"empty data path", func(r *supervisorStartRequest) { r.tasks[0].Data[0].Path = "" }, "data path is required"},
		{"zero timeout", func(r *supervisorStartRequest) { r.executionTimeout = 0 }, "executionTimeout must be positive"},
		{"empty backgroundRoot", func(r *supervisorStartRequest) { r.backgroundRoot = "" }, "backgroundRoot is required"},
		{"empty admissionRoot", func(r *supervisorStartRequest) { r.admissionRoot = "" }, "admissionRoot is required"},
		{"zero limit", func(r *supervisorStartRequest) { r.maxModelWorkers = 0 }, "maxModelWorkers must be positive"},
		{"worktree invalid name", func(r *supervisorStartRequest) { r.worktree.Name = "Bad!" }, "invalid name"},
		{"worktree branch mismatch", func(r *supervisorStartRequest) { r.worktree.Branch = "main" }, "branch must be run/"},
		{"empty pi executable", func(r *supervisorStartRequest) { r.piExecutable = "" }, "piExecutable is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validStartRequest()
			tt.mutate(&req)
			_, err := encodeSupervisorStartRequest(req)
			if err == nil {
				t.Fatalf("encode succeeded, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestSupervisorStartRequestDecodedOwnsMemory checks that fromJSON clones
// the wire's verify, write, data, and worktree memory: mutating the wire
// after conversion leaves the decoded request untouched.
func TestSupervisorStartRequestDecodedOwnsMemory(t *testing.T) {
	wire := validStartRequest().toJSON()
	decoded, err := wire.fromJSON()
	if err != nil {
		t.Fatalf("fromJSON: %v", err)
	}

	wire.Verify[0] = "changed"
	wire.Tasks[0].Writes[0] = "changed"
	wire.Tasks[0].Data[0].Content[0] = 0xEE
	wire.Worktree.Name = "changed"

	if decoded.verify[0] != "go" {
		t.Fatalf("decoded verify aliased wire: %q", decoded.verify[0])
	}
	if decoded.tasks[0].Writes.Paths[0] != "a.txt" {
		t.Fatalf("decoded writes aliased wire: %q", decoded.tasks[0].Writes.Paths[0])
	}
	if decoded.tasks[0].Data[0].Content[0] != 0x00 {
		t.Fatalf("decoded data aliased wire: %#x", decoded.tasks[0].Data[0].Content[0])
	}
	if decoded.worktree.Name != "issue-204" {
		t.Fatalf("decoded worktree aliased wire: %q", decoded.worktree.Name)
	}
}

// TestSupervisorStartRequestEncodedBytesStableAfterMutation checks that
// encoded bytes are a snapshot: mutating the source request through its
// own slices afterwards does not change what the earlier encoding decodes
// to.
func TestSupervisorStartRequestEncodedBytesStableAfterMutation(t *testing.T) {
	req := validStartRequest()
	encoded, err := encodeSupervisorStartRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	req.verify[0] = "changed"
	req.tasks[0].Writes.Paths[0] = "changed"
	req.tasks[0].Data[0].Content[0] = 0xEE
	req.worktree.Name = "changed"

	back, err := decodeSupervisorStartRequest(encoded)
	if err != nil {
		t.Fatalf("decode previously encoded bytes: %v", err)
	}
	if back.verify[0] != "go" {
		t.Fatalf("encoded bytes changed with source: verify %q", back.verify[0])
	}
	if back.tasks[0].Writes.Paths[0] != "a.txt" {
		t.Fatalf("encoded bytes changed with source: writes %q", back.tasks[0].Writes.Paths[0])
	}
	if back.tasks[0].Data[0].Content[0] != 0x00 {
		t.Fatalf("encoded bytes changed with source: data %#x", back.tasks[0].Data[0].Content[0])
	}
	if back.worktree.Name != "issue-204" {
		t.Fatalf("encoded bytes changed with source: worktree %q", back.worktree.Name)
	}
}

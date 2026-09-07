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
	"github.com/arasovic/pi-worker/internal/run"
	"github.com/arasovic/pi-worker/internal/runlog"
	"github.com/arasovic/pi-worker/internal/worktree"
)

// supervisorStartSchemaVersion is the only wire schema version accepted
// and produced for a supervisor start request.
const supervisorStartSchemaVersion = 1

// supervisorStartRequest is one accepted run as handed to the supervisor
// process: everything the supervisor needs to start the run in a private
// workspace without re-deriving anything from the parent process.
type supervisorStartRequest struct {
	runID            string
	acceptedAt       time.Time
	workspace        string
	tasks            []run.Task
	verify           []string
	executionTimeout time.Duration
	backgroundRoot   string
	admissionRoot    string
	maxModelWorkers  int
	worktree         *worktree.Prepared
	piExecutable     string
	debug            bool
}

// supervisorStartRequestJSON is the wire shape of a start request. Data
// content is []byte so encoding/json provides standard base64.
type supervisorStartRequestJSON struct {
	SchemaVersion    int           `json:"schemaVersion"`
	RunID            string        `json:"runId"`
	AcceptedAt       time.Time     `json:"acceptedAt"`
	Workspace        string        `json:"workspace"`
	Tasks            []taskJSON    `json:"tasks"`
	Verify           []string      `json:"verify,omitempty"`
	ExecutionTimeout string        `json:"executionTimeout"`
	BackgroundRoot   string        `json:"backgroundRoot"`
	AdmissionRoot    string        `json:"admissionRoot"`
	MaxModelWorkers  int           `json:"maxModelWorkers"`
	Worktree         *worktreeJSON `json:"worktree,omitempty"`
	PiExecutable     string        `json:"piExecutable"`
	Debug            bool          `json:"debug"`
}

// taskJSON keeps the write declaration's Declared flag separate from the
// paths on the wire, mirroring run.WriteDeclaration.
type taskJSON struct {
	Prompt         string         `json:"prompt"`
	Model          string         `json:"model"`
	ThinkingLevel  string         `json:"thinkingLevel,omitempty"`
	WritesDeclared bool           `json:"writesDeclared"`
	Writes         []string       `json:"writes,omitempty"`
	Data           []dataFileJSON `json:"data,omitempty"`
}

// dataFileJSON is one task material file on the wire: encoding/json
// encodes and decodes Content as standard base64.
type dataFileJSON struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
}

// worktreeJSON is the prepared worktree handed along with the request.
type worktreeJSON struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Branch string `json:"branch"`
	Head   string `json:"head"`
}

// encodeSupervisorStartRequest validates req and returns its wire JSON.
func encodeSupervisorStartRequest(req supervisorStartRequest) ([]byte, error) {
	if err := validateSupervisorStartRequest(req); err != nil {
		return nil, fmt.Errorf("encode supervisor start request: %w", err)
	}
	data, err := json.Marshal(req.toJSON())
	if err != nil {
		return nil, fmt.Errorf("encode supervisor start request: %w", err)
	}
	return data, nil
}

// decodeSupervisorStartRequest parses exactly one strict JSON document —
// unknown fields and trailing data are rejected — and validates the
// reconstructed request. The input must be valid UTF-8.
func decodeSupervisorStartRequest(data []byte) (supervisorStartRequest, error) {
	if !utf8.Valid(data) {
		return supervisorStartRequest{}, fmt.Errorf("decode supervisor start request: input is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var wire supervisorStartRequestJSON
	if err := dec.Decode(&wire); err != nil {
		return supervisorStartRequest{}, fmt.Errorf("decode supervisor start request: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return supervisorStartRequest{}, fmt.Errorf("decode supervisor start request: trailing data after document")
		}
		return supervisorStartRequest{}, fmt.Errorf("decode supervisor start request: %w", err)
	}
	if wire.SchemaVersion != supervisorStartSchemaVersion {
		return supervisorStartRequest{}, fmt.Errorf("decode supervisor start request: schemaVersion must be %d, got %d", supervisorStartSchemaVersion, wire.SchemaVersion)
	}
	req, err := wire.fromJSON()
	if err != nil {
		return supervisorStartRequest{}, fmt.Errorf("decode supervisor start request: %w", err)
	}
	if err := validateSupervisorStartRequest(req); err != nil {
		return supervisorStartRequest{}, fmt.Errorf("decode supervisor start request: %w", err)
	}
	return req, nil
}

// validateSupervisorStartRequest is the single validator shared by encode
// and decode; it checks the domain struct, never the wire shape.
func validateSupervisorStartRequest(req supervisorStartRequest) error {
	if req.acceptedAt.IsZero() {
		return fmt.Errorf("acceptedAt is required")
	}
	if req.acceptedAt.Location() != time.UTC {
		return fmt.Errorf("acceptedAt must be in UTC, got %s", req.acceptedAt.Location())
	}
	if err := runlog.ValidateRunID(req.runID, req.acceptedAt); err != nil {
		return fmt.Errorf("runId: %w", err)
	}
	if req.workspace == "" {
		return fmt.Errorf("workspace is required")
	}
	if req.backgroundRoot == "" {
		return fmt.Errorf("backgroundRoot is required")
	}
	if req.admissionRoot == "" {
		return fmt.Errorf("admissionRoot is required")
	}
	if req.piExecutable == "" {
		return fmt.Errorf("piExecutable is required")
	}
	if req.executionTimeout <= 0 {
		return fmt.Errorf("executionTimeout must be positive, got %s", req.executionTimeout)
	}
	if req.maxModelWorkers <= 0 {
		return fmt.Errorf("maxModelWorkers must be positive, got %d", req.maxModelWorkers)
	}
	if len(req.tasks) == 0 {
		return fmt.Errorf("at least one task is required")
	}
	if len(req.tasks) > run.MaxTasks {
		return fmt.Errorf("at most %d tasks are supported, got %d", run.MaxTasks, len(req.tasks))
	}
	for i, task := range req.tasks {
		if task.Model == "" {
			return fmt.Errorf("task %d: model is required", i+1)
		}
		provider, id, ok := splitModel(task.Model)
		if !ok || provider == "" || id == "" {
			return fmt.Errorf("task %d: model must be provider/id with non-empty halves", i+1)
		}
		if task.ThinkingLevel != "" {
			if _, ok := pi.ParseThinkingLevel(string(task.ThinkingLevel)); !ok {
				return fmt.Errorf("task %d: thinkingLevel is not a valid Pi thinking level", i+1)
			}
		}
		if !utf8.ValidString(task.Prompt) {
			return fmt.Errorf("task %d: prompt is not valid UTF-8", i+1)
		}
		if strings.TrimSpace(task.Prompt) == "" {
			return fmt.Errorf("task %d: prompt must not be blank", i+1)
		}
		if !task.Writes.Declared && len(task.Writes.Paths) > 0 {
			return fmt.Errorf("task %d: write paths require writesDeclared", i+1)
		}
		for _, file := range task.Data {
			if file.Path == "" {
				return fmt.Errorf("task %d: data path is required", i+1)
			}
		}
	}
	if err := run.ValidateWrites(req.tasks); err != nil {
		return err
	}
	if req.worktree != nil {
		if !worktree.ValidName(req.worktree.Name) {
			return fmt.Errorf("worktree: invalid name %q", req.worktree.Name)
		}
		if req.worktree.Branch != "run/"+req.worktree.Name {
			return fmt.Errorf("worktree: branch must be run/%s, got %q", req.worktree.Name, req.worktree.Branch)
		}
		if req.worktree.Path == "" {
			return fmt.Errorf("worktree: path is required")
		}
		if req.worktree.Head == "" {
			return fmt.Errorf("worktree: head is required")
		}
	}
	return nil
}

// toJSON converts the domain request to its wire shape.
func (req supervisorStartRequest) toJSON() supervisorStartRequestJSON {
	wire := supervisorStartRequestJSON{
		SchemaVersion:    supervisorStartSchemaVersion,
		RunID:            req.runID,
		AcceptedAt:       req.acceptedAt,
		Workspace:        req.workspace,
		Verify:           req.verify,
		ExecutionTimeout: req.executionTimeout.String(),
		BackgroundRoot:   req.backgroundRoot,
		AdmissionRoot:    req.admissionRoot,
		MaxModelWorkers:  req.maxModelWorkers,
		PiExecutable:     req.piExecutable,
		Debug:            req.debug,
	}
	if len(req.tasks) > 0 {
		wire.Tasks = make([]taskJSON, len(req.tasks))
		for i, task := range req.tasks {
			wire.Tasks[i] = taskJSON{
				Prompt:         task.Prompt,
				Model:          task.Model,
				ThinkingLevel:  string(task.ThinkingLevel),
				WritesDeclared: task.Writes.Declared,
				Writes:         task.Writes.Paths,
			}
			if len(task.Data) > 0 {
				wire.Tasks[i].Data = make([]dataFileJSON, len(task.Data))
				for j, file := range task.Data {
					wire.Tasks[i].Data[j] = dataFileJSON{Path: file.Path, Content: file.Content}
				}
			}
		}
	}
	if req.worktree != nil {
		wire.Worktree = &worktreeJSON{
			Name:   req.worktree.Name,
			Path:   req.worktree.Path,
			Branch: req.worktree.Branch,
			Head:   req.worktree.Head,
		}
	}
	return wire
}

// fromJSON converts the wire shape back to the domain request, cloning
// every slice, byte slice, and the worktree so the result shares nothing
// with the decoded wire value.
func (wire supervisorStartRequestJSON) fromJSON() (supervisorStartRequest, error) {
	d, err := time.ParseDuration(wire.ExecutionTimeout)
	if err != nil {
		return supervisorStartRequest{}, fmt.Errorf("executionTimeout is not a valid duration: %q", wire.ExecutionTimeout)
	}
	req := supervisorStartRequest{
		runID:            wire.RunID,
		acceptedAt:       wire.AcceptedAt,
		workspace:        wire.Workspace,
		verify:           cloneStrings(wire.Verify),
		executionTimeout: d,
		backgroundRoot:   wire.BackgroundRoot,
		admissionRoot:    wire.AdmissionRoot,
		maxModelWorkers:  wire.MaxModelWorkers,
		piExecutable:     wire.PiExecutable,
		debug:            wire.Debug,
	}
	if len(wire.Tasks) > 0 {
		req.tasks = make([]run.Task, len(wire.Tasks))
		for i, task := range wire.Tasks {
			req.tasks[i] = run.Task{
				Prompt:        task.Prompt,
				Model:         task.Model,
				ThinkingLevel: pi.ThinkingLevel(task.ThinkingLevel),
				Writes:        run.WriteDeclaration{Declared: task.WritesDeclared, Paths: cloneStrings(task.Writes)},
			}
			if len(task.Data) > 0 {
				req.tasks[i].Data = make([]run.DataFile, len(task.Data))
				for j, file := range task.Data {
					req.tasks[i].Data[j] = run.DataFile{Path: file.Path, Content: cloneBytes(file.Content)}
				}
			}
		}
	}
	if wire.Worktree != nil {
		req.worktree = &worktree.Prepared{
			Name:   wire.Worktree.Name,
			Path:   wire.Worktree.Path,
			Branch: wire.Worktree.Branch,
			Head:   wire.Worktree.Head,
		}
	}
	return req, nil
}

// cloneStrings returns a copy of a string slice; nil stays nil.
func cloneStrings(src []string) []string {
	if src == nil {
		return nil
	}
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// cloneBytes returns a copy of a byte slice; nil stays nil.
func cloneBytes(src []byte) []byte {
	if src == nil {
		return nil
	}
	out := make([]byte, len(src))
	copy(out, src)
	return out
}

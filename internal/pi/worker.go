package pi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Worker result statuses.
const (
	StatusCompleted   = "completed"
	StatusFailed      = "failed"
	StatusTimedOut    = "timed-out"
	StatusCancelled   = "cancelled"
	StatusUnavailable = "unavailable"
	StatusError       = "error"
)

// WorkerRequest describes one foreground worker invocation.
type WorkerRequest struct {
	Model     string
	Prompt    string
	Workspace string
	// WorkerID is operational metadata used only to label debug lines;
	// the zero value (direct callers) defaults to worker 1, and the
	// controller passes 1..N. It never appears in results or JSON.
	WorkerID int
	// Debug is the lifecycle sink; nil disables all debug logging.
	Debug *DebugSink
}

// WorkerResult is the concise result of one worker invocation.
type WorkerResult struct {
	Model       string `json:"model"`
	Explanation string `json:"explanation,omitempty"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
}

// Worker runs one foreground worker through Pi JSONL RPC.
type Worker interface {
	Run(context.Context, WorkerRequest) WorkerResult
}

// DefaultWorker launches the host pi executable and drives the documented
// worker lifecycle.
type DefaultWorker struct {
	executable string
}

// New returns a Worker that launches the given host pi executable.
func New(executable string) Worker {
	return &DefaultWorker{executable: executable}
}

// Run validates the request, starts pi in the workspace, verifies the exact
// provider/model against get_available_models, activates it with set_model,
// submits the prompt, waits for agent_settled, and returns the final
// assistant text. It never silently falls back to another model.
func (w *DefaultWorker) Run(ctx context.Context, req WorkerRequest) (result WorkerResult) {
	if req.Model == "" {
		return WorkerResult{Status: StatusFailed, Error: "model is required"}
	}
	if req.Prompt == "" {
		return WorkerResult{Model: req.Model, Status: StatusFailed, Error: "prompt is required"}
	}
	if req.Workspace == "" {
		return WorkerResult{Model: req.Model, Status: StatusFailed, Error: "workspace is required"}
	}
	provider, id, ok := splitModelSelector(req.Model)
	if !ok {
		return WorkerResult{Model: req.Model, Status: StatusFailed, Error: fmt.Sprintf("invalid model selector %q: expected exact provider/model", req.Model)}
	}
	if err := ctx.Err(); err != nil {
		// An already-expired deadline must surface as timed-out (exit 7
		// path) and an already-cancelled context as cancellation (exit 8
		// path); either way the host executable must not launch.
		if errors.Is(err, context.DeadlineExceeded) {
			return WorkerResult{Model: req.Model, Status: StatusTimedOut, Error: fmt.Sprintf("timed out: %v", err)}
		}
		return WorkerResult{Model: req.Model, Status: StatusCancelled, Error: fmt.Sprintf("cancelled: %v", err)}
	}
	debug := req.Debug.Worker(workerID(req.WorkerID))
	defer func() {
		debug.Log("status="+result.Status, "total="+debug.Elapsed().Round(time.Millisecond).String())
	}()
	debug.Log(debugStarting, "provider="+provider, "model="+id)

	proc, err := NewProcess(w.executable, req.Workspace)
	if err != nil {
		return WorkerResult{Model: req.Model, Status: StatusUnavailable, Error: err.Error()}
	}
	defer proc.Close()

	if err := proc.Start(ctx); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
			// A start failure caused by an expired deadline is a timeout
			// (exit 7 path), not a cancellation or readiness failure.
			return WorkerResult{Model: req.Model, Status: StatusTimedOut, Error: fmt.Sprintf("timed out: %v", err)}
		case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
			return WorkerResult{Model: req.Model, Status: StatusCancelled, Error: fmt.Sprintf("cancelled: %v", err)}
		}
		return WorkerResult{Model: req.Model, Status: StatusUnavailable, Error: fmt.Sprintf("start pi: %v", err)}
	}

	client := NewClient(proc.Stdin(), proc.Stdout(), nil, debug)

	models, err := client.GetAvailableModels(ctx)
	if err != nil {
		return w.classify(req.Model, ctx, err)
	}
	found := false
	for _, model := range models {
		if model.Provider == provider && model.ID == id {
			found = true
			break
		}
	}
	if !found {
		return WorkerResult{Model: req.Model, Status: StatusUnavailable, Error: fmt.Sprintf("model %q is not in the available catalog; no fallback attempted", req.Model)}
	}
	if err := client.SetModel(ctx, provider, id); err != nil {
		return w.classify(req.Model, ctx, err)
	}
	if err := client.Prompt(ctx, req.Prompt); err != nil {
		return w.classify(req.Model, ctx, err)
	}
	if err := client.WaitSettled(ctx); err != nil {
		return w.classify(req.Model, ctx, err)
	}
	text, err := client.GetLastAssistantText(ctx)
	if err != nil {
		return w.classify(req.Model, ctx, err)
	}
	if strings.TrimSpace(text) == "" {
		return WorkerResult{Model: req.Model, Status: StatusFailed, Error: "agent settled without producing final text"}
	}
	return WorkerResult{Model: req.Model, Status: StatusCompleted, Explanation: text}
}

// workerID maps an unset or invalid worker identity onto worker 1: direct
// callers leave WorkerID zero and the controller passes 1..N.
func workerID(id int) int {
	if id <= 0 {
		return 1
	}
	return id
}

// splitModelSelector parses an exact provider/model selector.
func splitModelSelector(selector string) (provider, id string, ok bool) {
	provider, id, found := strings.Cut(selector, "/")
	if !found || provider == "" || id == "" || strings.Contains(id, "/") {
		return "", "", false
	}
	if strings.ContainsAny(selector, ": \t\r\n") {
		return "", "", false
	}
	return provider, id, true
}

// classify maps an RPC failure onto a worker status, honoring context
// cancellation and timeout before protocol classification.
func (w *DefaultWorker) classify(model string, ctx context.Context, err error) WorkerResult {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return WorkerResult{Model: model, Status: StatusTimedOut, Error: fmt.Sprintf("timed out: %v", err)}
	case errors.Is(ctx.Err(), context.Canceled):
		return WorkerResult{Model: model, Status: StatusCancelled, Error: fmt.Sprintf("cancelled: %v", err)}
	}
	var protocolErr *ProtocolError
	var readinessErr *ReadinessError
	var modelErr *ModelUnavailableError
	var taskErr *TaskError
	switch {
	case errors.As(err, &protocolErr):
		return WorkerResult{Model: model, Status: StatusError, Error: protocolErr.Error()}
	case errors.As(err, &readinessErr):
		return WorkerResult{Model: model, Status: StatusUnavailable, Error: readinessErr.Error()}
	case errors.As(err, &modelErr):
		return WorkerResult{Model: model, Status: StatusUnavailable, Error: modelErr.Error()}
	case errors.As(err, &taskErr):
		return WorkerResult{Model: model, Status: StatusFailed, Error: taskErr.Error()}
	default:
		return WorkerResult{Model: model, Status: StatusError, Error: fmt.Sprintf("worker failure: %v", err)}
	}
}

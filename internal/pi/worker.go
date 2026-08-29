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
	Model         string
	ThinkingLevel ThinkingLevel
	Prompt        string
	Workspace     string
	// WorkerID is operational metadata used only to label debug lines;
	// the zero value (direct callers) defaults to worker 1, and the
	// controller passes 1..N. It never appears in results or JSON.
	WorkerID int
	// Debug is the lifecycle sink; nil disables all debug logging.
	Debug *DebugSink
}

// DataFile reports one file carried into a worker's prompt as material:
// the path, composed into the prompt as the section label, the byte
// count of how much content was carried, and the SHA-256 of the content
// as read, which identifies which content was carried. It is never
// produced by the worker — the worker receives only the composed prompt
// and has no knowledge of data files — so the run layer records it in
// the result from what it composed. Content itself is never reported.
type DataFile struct {
	Path   string `json:"path"`
	Bytes  int    `json:"byteCount"`
	SHA256 string `json:"sha256"`
}

// WorkerResult is the concise result of one worker invocation.
type WorkerResult struct {
	Model                  string        `json:"model"`
	RequestedThinkingLevel ThinkingLevel `json:"requestedThinkingLevel,omitempty"`
	ThinkingLevel          ThinkingLevel `json:"thinkingLevel,omitempty"`
	ThinkingFallback       bool          `json:"thinkingFallback,omitempty"`
	Warning                string        `json:"warning,omitempty"`
	Explanation            string        `json:"explanation,omitempty"`
	Status                 string        `json:"status"`
	Error                  string        `json:"error,omitempty"`
	// DataFiles lists each file carried into the prompt as material,
	// populated by the run layer from what it composed; absent when the
	// task carried no material.
	DataFiles []DataFile `json:"data,omitempty"`
	// Usage is the summed usage of the worker's assistant messages as Pi
	// reported it: token counts and US-dollar cost figures pass through
	// unchanged, and pi-worker derives no price of its own. It is nil
	// when no message reported a non-zero usage figure — never measured,
	// or only zeros reported — so a provider that reports no numbers
	// leaves the field absent rather than claiming a free run.
	Usage *Usage `json:"usage,omitempty"`
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
	thinking := thinkingOutcome{requested: req.ThinkingLevel}
	// Usage is measured on every path: every return funnels through
	// withThinking, so a failed or timed-out run still reports what it
	// spent. Validation failures before the client is created snapshot
	// nil: no frame was observed, so usage is absent rather than zero.
	usage := &usageAccumulator{}
	withThinking := func(result WorkerResult) WorkerResult {
		result.Usage = usage.snapshot()
		return thinking.apply(result)
	}
	if req.ThinkingLevel != "" {
		if parsed, ok := ParseThinkingLevel(string(req.ThinkingLevel)); !ok || parsed != req.ThinkingLevel {
			return withThinking(WorkerResult{Model: req.Model, Status: StatusFailed, Error: fmt.Sprintf("invalid thinking level %q", req.ThinkingLevel)})
		}
	}
	if req.Prompt == "" {
		return withThinking(WorkerResult{Model: req.Model, Status: StatusFailed, Error: "prompt is required"})
	}
	if req.Workspace == "" {
		return withThinking(WorkerResult{Model: req.Model, Status: StatusFailed, Error: "workspace is required"})
	}
	provider, id, ok := splitModelSelector(req.Model)
	if !ok {
		return withThinking(WorkerResult{Model: req.Model, Status: StatusFailed, Error: fmt.Sprintf("invalid model selector %q: expected exact provider/model", req.Model)})
	}
	if err := ctx.Err(); err != nil {
		// An already-expired deadline must surface as timed-out (exit 7
		// path) and an already-cancelled context as cancellation (exit 8
		// path); either way the host executable must not launch.
		if errors.Is(err, context.DeadlineExceeded) {
			return withThinking(WorkerResult{Model: req.Model, Status: StatusTimedOut, Error: fmt.Sprintf("timed out: %v", err)})
		}
		return withThinking(WorkerResult{Model: req.Model, Status: StatusCancelled, Error: fmt.Sprintf("cancelled: %v", err)})
	}
	debug := req.Debug.Worker(workerID(req.WorkerID))
	stopHeartbeat := func() {}
	defer func() {
		stopHeartbeat()
		debug.LogTerminal("status="+result.Status, "total="+debug.Elapsed().Round(time.Millisecond).String())
	}()
	requestedDebug := "default"
	if req.ThinkingLevel != "" {
		requestedDebug = string(req.ThinkingLevel)
	}
	debug.Log(debugStarting, "provider="+provider, "model="+id, "thinking-requested="+requestedDebug)

	proc, err := NewProcess(w.executable, req.Workspace)
	if err != nil {
		return withThinking(WorkerResult{Model: req.Model, Status: StatusUnavailable, Error: err.Error()})
	}
	defer proc.Close()

	if err := proc.Start(ctx); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
			// A start failure caused by an expired deadline is a timeout
			// (exit 7 path), not a cancellation or readiness failure.
			return withThinking(WorkerResult{Model: req.Model, Status: StatusTimedOut, Error: fmt.Sprintf("timed out: %v", err)})
		case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
			return withThinking(WorkerResult{Model: req.Model, Status: StatusCancelled, Error: fmt.Sprintf("cancelled: %v", err)})
		}
		return withThinking(WorkerResult{Model: req.Model, Status: StatusUnavailable, Error: fmt.Sprintf("start pi: %v", err)})
	}
	stopHeartbeat = debug.startHeartbeat(proc.Running)

	client := NewClient(proc.Stdin(), proc.Stdout(), usage, debug)

	models, err := client.GetAvailableModels(ctx)
	if err != nil {
		return withThinking(w.classify(req.Model, ctx, err))
	}
	found := false
	for _, model := range models {
		if model.Provider == provider && model.ID == id {
			found = true
			break
		}
	}
	if !found {
		return withThinking(WorkerResult{Model: req.Model, Status: StatusUnavailable, Error: fmt.Sprintf("model %q is not in the available catalog; no fallback attempted", req.Model)})
	}
	if err := client.SetModel(ctx, provider, id); err != nil {
		return withThinking(w.classify(req.Model, ctx, err))
	}
	baseline, err := client.GetState(ctx)
	if err != nil {
		return withThinking(w.classify(req.Model, ctx, err))
	}
	if err := validateStateModel(baseline, provider, id); err != nil {
		return withThinking(w.classify(req.Model, ctx, err))
	}
	thinking.effective = baseline.ThinkingLevel

	if req.ThinkingLevel != "" {
		levels, err := client.GetAvailableThinkingLevels(ctx)
		if err != nil {
			return withThinking(w.classify(req.Model, ctx, err))
		}
		if !thinkingLevelsContain(levels, req.ThinkingLevel) {
			thinking.fallback = true
			thinking.warning = thinkingFallbackWarning(req.ThinkingLevel, baseline.ThinkingLevel, "unavailable")
		} else {
			err := client.SetThinkingLevel(ctx, req.ThinkingLevel)
			var rejected *ThinkingLevelRejectedError
			switch {
			case err == nil:
				confirmed, stateErr := client.GetState(ctx)
				if stateErr != nil {
					return withThinking(w.classify(req.Model, ctx, stateErr))
				}
				if stateErr := validateStateModel(confirmed, provider, id); stateErr != nil {
					return withThinking(w.classify(req.Model, ctx, stateErr))
				}
				if confirmed.ThinkingLevel != req.ThinkingLevel {
					return withThinking(w.classify(req.Model, ctx, newProtocolError("get_state did not confirm requested thinking level")))
				}
				thinking.effective = confirmed.ThinkingLevel
			case errors.As(err, &rejected):
				confirmed, stateErr := client.GetState(ctx)
				if stateErr != nil {
					return withThinking(w.classify(req.Model, ctx, stateErr))
				}
				if stateErr := validateStateModel(confirmed, provider, id); stateErr != nil {
					return withThinking(w.classify(req.Model, ctx, stateErr))
				}
				if confirmed.ThinkingLevel != baseline.ThinkingLevel {
					return withThinking(w.classify(req.Model, ctx, newProtocolError("rejected thinking change did not preserve Pi default")))
				}
				thinking.fallback = true
				thinking.warning = thinkingFallbackWarning(req.ThinkingLevel, baseline.ThinkingLevel, "rejected")
			default:
				return withThinking(w.classify(req.Model, ctx, err))
			}
		}
	}
	debugFields := []string{"thinking-effective=" + string(thinking.effective)}
	if thinking.fallback {
		debugFields = append(debugFields, "thinking-fallback=true")
	}
	debug.Log(debugThinking, debugFields...)
	if err := client.Prompt(ctx, req.Prompt); err != nil {
		return withThinking(w.classify(req.Model, ctx, err))
	}
	if err := client.WaitSettled(ctx); err != nil {
		return withThinking(w.classify(req.Model, ctx, err))
	}
	text, err := client.GetLastAssistantText(ctx)
	if err != nil {
		return withThinking(w.classify(req.Model, ctx, err))
	}
	if strings.TrimSpace(text) == "" {
		return withThinking(WorkerResult{Model: req.Model, Status: StatusFailed, Error: "agent settled without producing final text"})
	}
	return withThinking(WorkerResult{Model: req.Model, Status: StatusCompleted, Explanation: text})
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
	canonical, componentsOK := ExactModelSelector(provider, id)
	if !found || !componentsOK || canonical != selector {
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

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

// ProcessObserver is told the identity of the process one worker
// started, at the moment it starts: the worker id it ran under and the
// launched process's pid. It is the run-level passenger that carries
// the identity out of the worker while the run is in flight — the
// identity exists only then, so it can never be recovered later.
type ProcessObserver func(workerID int, pid int)

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
	// OnProcessStart, when non-nil, is called once per worker with the
	// identity of the process it launched, immediately after the process
	// starts. It is separate from Debug: the record must be written on
	// every run, while debug output is off unless requested.
	OnProcessStart ProcessObserver
	// Controls, when non-nil, is a serial channel of typed worker controls
	// serviced while the model turn is in flight, between Prompt and the
	// terminal agent_settled event. The owning run drains exactly one
	// in-flight control at a time: a steer or abort request is written,
	// its correlated response is consumed, then the next control is read.
	// nil preserves the exact WaitSettled behavior. The worker reads
	// exactly one control at a time and never pipelines them.
	Controls <-chan WorkerControl
}

// WorkerControlKind names one typed control the worker can service during
// the in-flight wait. The values are exact wire commands: WorkerControlSteer
// carries a steering message and WorkerControlAbort carries no payload.
type WorkerControlKind string

const (
	WorkerControlSteer WorkerControlKind = "steer"
	WorkerControlAbort WorkerControlKind = "abort"
)

// WorkerControl is one typed control request serviced by the worker while
// the model turn is in flight. The Kind selects the wire command, Message
// is the steer payload (ignored for abort), and Result receives one
// deterministic error outcome for this control only — nil on success or
// a typed *TaskError on a correlated rejection, or one of context /
// transport / protocol errors when the wait ends. A valid Result channel
// is non-nil, buffered, and empty when submitted; the worker owns the
// single send and delivers the outcome via a blocking send.
type WorkerControl struct {
	Kind    WorkerControlKind
	Message string
	Result  chan<- error
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
	// PartialExplanation is the assistant text observed before the run ended
	// without a final text: the retained recent text_delta content captured
	// while it streamed. It is at most MaxFrameBytes UTF-8 bytes; when the
	// stream exceeds that shared aggregate budget, older text is evicted on a
	// UTF-8 boundary. It is present only when explanation is absent — a run
	// that ended mid-message still reports what it produced, while a completed
	// run never carries it — so a consumer reading explanation can always
	// assume the model finished.
	PartialExplanation string `json:"partialExplanation,omitempty"`
	Status             string `json:"status"`
	Error              string `json:"error,omitempty"`
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
	// Usage is measured on every path after the model guard: every
	// return funnels through withThinking, so a failed or timed-out run
	// still reports what it spent. Only the model-required return above
	// is outside that rule. Validation failures before the client is
	// created snapshot nil: no frame was observed, so usage is absent
	// rather than zero.
	usage := &usageAccumulator{}
	// transcript holds the last assistant message's text as it streams,
	// the partial counterpart of explanation: withThinking reports it when
	// the run ends without a final text, so a failed or timed-out run
	// still carries the text it produced. It obeys the same rule as usage:
	// every return after the model guard funnels through withThinking.
	// Validation failures before the client is created snapshot empty: no
	// frame was observed.
	transcript := &transcriptAccumulator{}
	withThinking := func(result WorkerResult) WorkerResult {
		result.Usage = usage.snapshot()
		if result.Explanation == "" {
			result.PartialExplanation = transcript.snapshot()
		}
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
		return withThinking(WorkerResult{Model: req.Model, Status: StatusFailed, Error: fmt.Sprintf("invalid model selector %q: expected provider/model", req.Model)})
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

	var (
		successProc               *Process
		client                    *Client
		successThinking           thinkingOutcome
		startupWarning            string
		lastRetryableFailureClass string
	)
	for attempt := 1; attempt <= 3; attempt++ {
		debug.Log(debugStarting, "provider="+provider, "model="+id, "thinking-requested="+requestedDebug)
		attemptThinking := thinkingOutcome{requested: req.ThinkingLevel}

		proc, err := NewProcess(w.executable, req.Workspace)
		if err != nil {
			if attempt < 3 {
				lastRetryableFailureClass = StatusUnavailable
				continue
			}
			thinking = attemptThinking
			return withThinking(WorkerResult{Model: req.Model, Status: StatusUnavailable, Error: err.Error()})
		}

		if err := proc.Start(ctx); err != nil {
			_ = proc.Close()
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				thinking = attemptThinking
				return withThinking(w.classify(req.Model, ctx, err))
			}
			if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
				thinking = attemptThinking
				return withThinking(w.classify(req.Model, ctx, err))
			}
			failure := WorkerResult{Model: req.Model, Status: StatusUnavailable, Error: fmt.Sprintf("start pi: %v", err)}
			if attempt < 3 {
				lastRetryableFailureClass = failure.Status
				continue
			}
			thinking = attemptThinking
			return withThinking(failure)
		}

		attemptStop := debug.startHeartbeat(proc.Running)
		stopHeartbeat = attemptStop
		if req.OnProcessStart != nil {
			// The observer receives the raw WorkerID, never the debug-label
			// normalization on the line above: the record must pair the pid
			// with the identity the caller assigned, not the label a direct
			// caller's zero value would map to worker 1. The pid guard
			// mirrors Pid's condition: no identity exists before the child
			// starts, and Pid never reports one after it is reaped.
			if pid := proc.Pid(); pid != 0 {
				req.OnProcessStart(req.WorkerID, pid)
			}
		}

		client = NewClient(proc.Stdin(), proc.Stdout(), eventHandlers{usage, transcript}, debug)
		attemptThinking, failure, retryable, ok := w.prePromptAttempt(ctx, req, provider, id, client)
		if ok {
			successProc = proc
			successThinking = attemptThinking
			if attempt > 1 {
				startupWarning = startupRetryWarning(attempt, lastRetryableFailureClass)
			}
			break
		}

		thinking = attemptThinking
		attemptStop()
		_ = client.Close()
		_ = proc.Close()
		if retryable && attempt < 3 {
			lastRetryableFailureClass = failure.Status
			continue
		}
		return withThinking(failure)
	}
	if successProc == nil {
		return withThinking(WorkerResult{Model: req.Model, Status: StatusUnavailable, Error: "startup attempts exhausted"})
	}
	defer successProc.Close()
	defer client.Close()

	thinking = successThinking
	if startupWarning != "" {
		if thinking.warning != "" {
			thinking.warning = startupWarning + "; " + thinking.warning
		} else {
			thinking.warning = startupWarning
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
	// The wait between Prompt and the terminal agent_settled event is the
	// single owned FrameReader consumer window: when Controls is nil the
	// existing WaitSettled path runs unchanged; when Controls is non-nil
	// WaitSettledControlled selects between pumped frames and one typed
	// control at a time, keeping the same sole-consumer invariant during
	// the model turn.
	if req.Controls == nil {
		if err := client.WaitSettled(ctx); err != nil {
			return withThinking(w.classify(req.Model, ctx, err))
		}
	} else {
		if err := client.WaitSettledControlled(ctx, req.Controls); err != nil {
			return withThinking(w.classify(req.Model, ctx, err))
		}
	}
	text, err := client.GetLastAssistantText(ctx)
	if err != nil {
		return withThinking(w.classify(req.Model, ctx, err))
	}
	if transcript.assistantError() {
		// stopReason is Pi's stable classification of the assistant
		// message. Do not surface its accompanying errorMessage: that
		// field is upstream-controlled prose and may carry secrets,
		// credentials, URLs, or unstable response bodies. An error stop
		// is not a completed answer even if the turn emitted some text;
		// withThinking preserves any such streamed text as partialExplanation.
		return withThinking(WorkerResult{Model: req.Model, Status: StatusFailed, Error: "upstream/model turn ended with an error"})
	}
	if strings.TrimSpace(text) == "" {
		return withThinking(WorkerResult{Model: req.Model, Status: StatusFailed, Error: "agent settled without producing final text"})
	}
	return withThinking(WorkerResult{Model: req.Model, Status: StatusCompleted, Explanation: text})
}

// prePromptAttempt drives the entire startup handshake before the prompt is
// submitted. It returns a filled thinking projection on success and a typed
// failure plus retryability on transient pre-prompt problems.
func (w *DefaultWorker) prePromptAttempt(ctx context.Context, req WorkerRequest, provider, id string, client *Client) (thinking thinkingOutcome, failure WorkerResult, retryable, ok bool) {
	thinking = thinkingOutcome{requested: req.ThinkingLevel}

	models, err := client.GetAvailableModels(ctx)
	if err != nil {
		return thinking, w.classify(req.Model, ctx, err), retryableStartupFailure(err), false
	}
	found := false
	for _, model := range models {
		if model.Provider == provider && model.ID == id {
			found = true
			break
		}
	}
	if !found {
		return thinking, WorkerResult{Model: req.Model, Status: StatusUnavailable, Error: fmt.Sprintf("model %q is not in the available catalog; no fallback attempted", req.Model)}, false, false
	}
	if err := client.SetModel(ctx, provider, id); err != nil {
		return thinking, w.classify(req.Model, ctx, err), retryableStartupFailure(err), false
	}
	baseline, err := client.GetState(ctx)
	if err != nil {
		return thinking, w.classify(req.Model, ctx, err), retryableStartupFailure(err), false
	}
	if err := validateStateModel(baseline, provider, id); err != nil {
		return thinking, w.classify(req.Model, ctx, err), false, false
	}
	thinking.effective = baseline.ThinkingLevel

	if req.ThinkingLevel != "" {
		levels, err := client.GetAvailableThinkingLevels(ctx)
		if err != nil {
			return thinking, w.classify(req.Model, ctx, err), retryableStartupFailure(err), false
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
					return thinking, w.classify(req.Model, ctx, stateErr), retryableStartupFailure(stateErr), false
				}
				if stateErr := validateStateModel(confirmed, provider, id); stateErr != nil {
					return thinking, w.classify(req.Model, ctx, stateErr), false, false
				}
				if confirmed.ThinkingLevel != req.ThinkingLevel {
					return thinking, w.classify(req.Model, ctx, newProtocolError("get_state did not confirm requested thinking level")), false, false
				}
				thinking.effective = confirmed.ThinkingLevel
			case errors.As(err, &rejected):
				confirmed, stateErr := client.GetState(ctx)
				if stateErr != nil {
					return thinking, w.classify(req.Model, ctx, stateErr), retryableStartupFailure(stateErr), false
				}
				if stateErr := validateStateModel(confirmed, provider, id); stateErr != nil {
					return thinking, w.classify(req.Model, ctx, stateErr), false, false
				}
				if confirmed.ThinkingLevel != baseline.ThinkingLevel {
					return thinking, w.classify(req.Model, ctx, newProtocolError("rejected thinking change did not preserve Pi default")), false, false
				}
				thinking.fallback = true
				thinking.warning = thinkingFallbackWarning(req.ThinkingLevel, baseline.ThinkingLevel, "rejected")
			default:
				return thinking, w.classify(req.Model, ctx, err), retryableStartupFailure(err), false
			}
		}
	}
	return thinking, WorkerResult{}, false, true
}

func startupRetryWarning(attempt int, priorFailureClass string) string {
	if attempt <= 1 || priorFailureClass == "" {
		return ""
	}
	return fmt.Sprintf("startup succeeded on attempt %d/3 after %s startup failure", attempt, priorFailureClass)
}

func retryableStartupFailure(err error) bool {
	var protocolErr *ProtocolError
	var readinessErr *ReadinessError
	var transportErr *transportError
	return errors.As(err, &transportErr) || errors.As(err, &readinessErr) || errors.As(err, &protocolErr)
}

// workerID maps an unset or invalid worker identity onto worker 1: direct
// callers leave WorkerID zero and the controller passes 1..N.
func workerID(id int) int {
	if id <= 0 {
		return 1
	}
	return id
}

// splitModelSelector parses an exact provider/model selector: the part
// before the first slash is the provider and the remainder is the id. It
// asks the one selector rule whether the halves name something — that rule
// and this parser must never disagree. The catalog-membership check that
// follows is what decides whether the name is usable.
func splitModelSelector(selector string) (provider, id string, ok bool) {
	provider, id, found := strings.Cut(selector, "/")
	if !found {
		return "", "", false
	}
	if _, ruleOK := ExactModelSelector(provider, id); !ruleOK {
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

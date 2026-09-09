package background

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/arasovic/pi-worker/internal/pi"
)

// workerHostAdapter is one concrete private pi.Worker: it executes one
// pi.WorkerRequest through exactly one private same-binary worker-host
// child and returns the child host's terminal pi.WorkerResult. It is the
// parent half of the private worker-host protocol; receiveWorkerHost is
// the child half.
//
// executable must name a binary that dispatches roleWorkerHost children
// (the pi-worker binary in production); piExecutable names the host pi
// executable the child host launches for the model run. The caller's
// per-worker context must carry the execution deadline: the remaining
// deadline is what the child host enforces as its execution timeout, so
// an unbounded context is rejected before any child exists.
type workerHostAdapter struct {
	executable   string // same-binary role executable spawning worker-host children
	piExecutable string // host pi executable the child host launches
}

// newWorkerHostAdapter returns the private worker-host pi.Worker adapter
// for the given role executable and pi executable.
func newWorkerHostAdapter(executable, piExecutable string) *workerHostAdapter {
	return &workerHostAdapter{executable: executable, piExecutable: piExecutable}
}

// Run executes one task through one private worker-host child. On
// platforms that cannot start role processes it answers with the
// existing errRoleProcessUnsupported result before any request
// validation. Otherwise the pre-launch guards mirror the executed
// pi.Worker's own validation with the same statuses and messages, so a
// request the worker could never run fails here without spawning a
// child; the context's deadline is the execution timeout carried to the
// child host, and cancellation closes ownership first so the host can
// finish its own Pi containment cleanup before it is reaped. Only the
// bounded fallback ever kills the host, and a force-killed host is
// reported with explicit cleanup uncertainty. Non-nil Controls are
// rejected explicitly: steering is not supported by the private worker
// host.
func (a *workerHostAdapter) Run(ctx context.Context, req pi.WorkerRequest) (result pi.WorkerResult) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Unsupported platforms can start no role process, so no request
	// could ever run there: answer with the existing unsupported
	// role-process result before any request validation, so an invalid
	// request can never masquerade as a validation failure where no
	// host exists. The guard is a build-tagged constant; the check is
	// free on platforms that support role processes.
	if !workerHostPlatformSupported {
		return pi.WorkerResult{Model: req.Model, Status: pi.StatusUnavailable, Error: fmt.Sprintf("start worker host: %v", errRoleProcessUnsupported)}
	}
	if req.Model == "" {
		return pi.WorkerResult{Status: pi.StatusFailed, Error: "model is required"}
	}
	if req.ThinkingLevel != "" {
		if parsed, ok := pi.ParseThinkingLevel(string(req.ThinkingLevel)); !ok || parsed != req.ThinkingLevel {
			return pi.WorkerResult{Model: req.Model, Status: pi.StatusFailed, Error: fmt.Sprintf("invalid thinking level %q", req.ThinkingLevel)}
		}
	}
	if req.Prompt == "" {
		return pi.WorkerResult{Model: req.Model, Status: pi.StatusFailed, Error: "prompt is required"}
	}
	if req.Workspace == "" {
		return pi.WorkerResult{Model: req.Model, Status: pi.StatusFailed, Error: "workspace is required"}
	}
	if provider, id, ok := strings.Cut(req.Model, "/"); !ok {
		return pi.WorkerResult{Model: req.Model, Status: pi.StatusFailed, Error: fmt.Sprintf("invalid model selector %q: expected provider/model", req.Model)}
	} else if _, ruleOK := pi.ExactModelSelector(provider, id); !ruleOK {
		return pi.WorkerResult{Model: req.Model, Status: pi.StatusFailed, Error: fmt.Sprintf("invalid model selector %q: expected provider/model", req.Model)}
	}
	if req.Controls != nil {
		// Controls/steering are deferred for the private worker host:
		// a non-nil control channel is rejected explicitly rather than
		// silently ignored.
		return pi.WorkerResult{Model: req.Model, Status: pi.StatusFailed, Error: "worker host: controls are not supported"}
	}
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return pi.WorkerResult{Model: req.Model, Status: pi.StatusTimedOut, Error: fmt.Sprintf("timed out: %v", err)}
		}
		return pi.WorkerResult{Model: req.Model, Status: pi.StatusCancelled, Error: fmt.Sprintf("cancelled: %v", err)}
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return pi.WorkerResult{Model: req.Model, Status: pi.StatusFailed, Error: "worker host: an execution deadline is required"}
	}
	executionTimeout := time.Until(deadline)
	if executionTimeout <= 0 {
		return pi.WorkerResult{Model: req.Model, Status: pi.StatusTimedOut, Error: "timed out: execution deadline expired before the worker host started"}
	}

	wireReq := workerHostRequest{
		// The wire carries the positive identity the debug-label mapping
		// would assign: the zero value of a direct caller defaults to
		// worker 1, matching the worker's own workerID normalization.
		workerID:         workerHostWorkerID(req.WorkerID),
		workspace:        req.Workspace,
		model:            req.Model,
		thinkingLevel:    req.ThinkingLevel,
		prompt:           req.Prompt,
		piExecutable:     a.piExecutable,
		executionTimeout: executionTimeout,
	}
	payload, err := encodeWorkerHostRequest(wireReq)
	if err != nil {
		return pi.WorkerResult{Model: req.Model, Status: pi.StatusFailed, Error: fmt.Sprintf("encode worker host request: %v", err)}
	}
	// The encoded payload is exactly the frame the child host would
	// receive, so a request larger than the private frame limit is
	// rejected here, before any child exists. No host may be spawned for
	// a request the frame protocol could never deliver.
	if len(payload) > privateFrameLimit {
		return pi.WorkerResult{Model: req.Model, Status: pi.StatusFailed, Error: fmt.Sprintf(
			"encode worker host request: request payload is %d bytes, exceeding the private frame limit of %d bytes",
			len(payload), privateFrameLimit)}
	}
	return a.execute(ctx, req, payload)
}

// workerHostWorkerID maps an unset or invalid worker identity onto
// worker 1, mirroring the worker's own label normalization: direct
// callers leave WorkerID zero and the supervisor passes 1..N.
func workerHostWorkerID(id int) int {
	if id <= 0 {
		return 1
	}
	return id
}

//go:build darwin || linux

package background

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"golang.org/x/sys/unix"

	"github.com/arasovic/pi-worker/internal/pi"
)

// workerHostWriteGrace bounds every response-frame write the child host
// performs: a broken or blocked response transport must never pin the
// host's cleanup or exit. The child host's request and response
// descriptors are opened non-blocking and pollable (openChildRolePipes),
// so the runtime poller enforces this absolute deadline: the write
// either completes, fails with EPIPE (the parent is gone), or times out
// within the grace — the host always finishes on its own, never by
// relying on the parent's bounded fallback kill. It is a variable only
// so in-process tests can shorten it.
var workerHostWriteGrace = 10 * time.Second

// workerHostPlatformSupported reports whether this platform can start
// role processes at all. workerHostAdapter.Run answers unsupported
// platforms with the existing errRoleProcessUnsupported result before
// any request validation.
const workerHostPlatformSupported = true

// workerHostCleanupBound bounds every parent-side wait on a host that
// has been asked to stop: after ownership loss the host must finish its
// own Pi containment cleanup and exit within this bound, and a host that
// reported its terminal result must exit within it. Only the bounded
// fallback kill may end the host early; a force-killed host is reported
// with explicit cleanup uncertainty. It is a variable only so tests can
// shorten it.
var workerHostCleanupBound = 30 * time.Second

// workerHostExchange is the outcome of one child-side worker-host
// exchange: the decoded request and the terminal worker result whose
// frame reached the wire. A read or decode failure before any run leaves
// request zero and result carrying the terminal failure frame that
// answered the exchange.
type workerHostExchange struct {
	request workerHostRequest
	result  pi.WorkerResult
}

// receiveWorkerHost executes one private worker-host exchange as the
// child half, taking ownership of the child role transport ends — the
// request reader on fd 3, the response writer on fd 4, and the ownership
// reader on fd 5 — and closing all three on every return. It reads
// exactly one bounded request frame and decodes one strict execution
// request, runs exactly one task through pi.New(req.piExecutable).Run,
// forwards every process-start notification the run reports, and sends
// exactly one terminal result frame. It never waits for a second frame
// or for starter EOF after the request; only ownership EOF cancels the
// run.
//
// The host context is cancelled the moment the ownership reader reaches
// EOF (or fails). The execution timeout bounds the run independently of
// the parent. Request EOF after the complete request does not cancel
// anything; a request that never arrives intact is answered with one
// terminal failure frame and no run.
func receiveWorkerHost(pipes *childRolePipes) (exchange workerHostExchange, err error) {
	if pipes == nil {
		return workerHostExchange{}, fmt.Errorf("receive worker host: pipes must not be nil")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The ownership watcher exits on owner loss or when the exchange
	// stops it. The ownership descriptor stays a blocking file that the
	// runtime cannot wake by closing, so the watcher polls the raw
	// descriptor in bounded slices and checks a stop channel between
	// slices instead of blocking in a read forever. On owner loss the
	// watcher cancels the host context and expires the request-read
	// deadline, so a request read that is still pending — a partial
	// request, or none at all — returns instead of pinning the exchange.
	// The stop/join/close defer order is preserved: the deferred stop
	// (registered last, so it runs first) ends the watcher within one
	// poll slice, the join (registered second) waits for it and collects
	// any read-deadline expiry error it recorded, and only then the
	// pipes close (registered first, so it runs last): no watcher
	// goroutine outlives the exchange in any mode, no descriptor is
	// closed while the watcher still polls it, and no expiry error is
	// silently dropped.
	ownershipFD := int(pipes.ownershipReader.Fd())
	// The expiry error is written by the watcher goroutine and read
	// after the join below, so it cannot race the pipes close and is
	// reported honestly instead of silently leaving the read pending.
	var requestReadDeadlineErr error
	expireRequestRead := func() {
		if pipes.requestReader == nil {
			return
		}
		if err := pipes.requestReader.SetReadDeadline(time.Now()); err != nil {
			requestReadDeadlineErr = fmt.Errorf("expire request read deadline after ownership loss: %w", err)
		}
	}
	watcherStop := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		watchWorkerHostOwnership(ownershipFD, cancel, expireRequestRead, watcherStop)
	}()
	defer func() {
		if closeErr := pipes.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	defer func() {
		<-watcherDone
		if requestReadDeadlineErr != nil {
			err = errors.Join(err, requestReadDeadlineErr)
		}
	}()
	defer close(watcherStop)

	// Read exactly one bounded request frame. Ownership loss while the
	// request is still pending cancels ctx and expires the request-read
	// deadline, so a partial request that never completes returns the
	// pending read instead of pinning the exchange; request EOF before
	// any request arrived is answered with one terminal failure frame
	// and no run. The read itself otherwise completes because the parent
	// either delivers the whole request or dies (EOF).
	payload, readErr := readFrame(pipes.requestReader, privateFrameLimit)
	if readErr != nil {
		status := pi.StatusFailed
		reason := fmt.Sprintf("read request frame: %v", readErr)
		if ctx.Err() != nil {
			status = pi.StatusCancelled
			reason = fmt.Sprintf("ownership lost before the request completed: %v", reason)
		}
		terminal := pi.WorkerResult{Status: status, Error: "worker host: " + reason}
		terminalPayload, encErr := encodeWorkerHostResult(terminal)
		if encErr != nil {
			return workerHostExchange{}, errors.Join(fmt.Errorf("receive worker host: %w", readErr), encErr)
		}
		if writeErr := writeWorkerHostResponse(pipes.responseWriter, terminalPayload); writeErr != nil {
			return workerHostExchange{}, errors.Join(
				fmt.Errorf("receive worker host: %w", readErr),
				fmt.Errorf("receive worker host: write terminal result frame: %w", writeErr))
		}
		return workerHostExchange{result: terminal}, nil
	}

	// Decode one strict execution request. A malformed request must
	// never launch Pi: the frame is answered with one terminal failure
	// frame and the exchange ends without a run.
	req, decodeErr := decodeWorkerHostRequest(payload)
	if decodeErr != nil {
		terminal := pi.WorkerResult{Status: pi.StatusFailed, Error: fmt.Sprintf("worker host: decode request frame: %v", decodeErr)}
		terminalPayload, encErr := encodeWorkerHostResult(terminal)
		if encErr != nil {
			return workerHostExchange{}, errors.Join(decodeErr, encErr)
		}
		if writeErr := writeWorkerHostResponse(pipes.responseWriter, terminalPayload); writeErr != nil {
			return workerHostExchange{}, errors.Join(
				decodeErr,
				fmt.Errorf("receive worker host: write terminal result frame: %w", writeErr))
		}
		return workerHostExchange{result: terminal}, nil
	}

	// The run context carries both cancellations: the ownership watcher
	// cancels on owner loss, and the execution timeout bounds the run
	// independently of the parent.
	runCtx, runCancel := context.WithTimeout(ctx, req.executionTimeout)
	defer runCancel()

	// Every process-start notification the worker reports is forwarded
	// as one response frame — including every startup-retry identity, in
	// launch order, before the terminal result. A notification write
	// failure means the response stream is broken — the frame may have
	// ended partially on the wire: the run is cancelled so the host
	// cleans Pi and exits, and no terminal frame is ever appended after
	// the partial or failed notification frame (see below).
	var notifyErr error
	notify := func(workerID, pid int) {
		if notifyErr != nil {
			return
		}
		frame, encErr := encodeWorkerHostProcessStart(workerID, pid)
		if encErr != nil {
			notifyErr = encErr
			cancel()
			return
		}
		if writeErr := writeWorkerHostResponse(pipes.responseWriter, frame); writeErr != nil {
			notifyErr = fmt.Errorf("write process-start notification: %w", writeErr)
			cancel()
		}
	}

	workerResult := pi.New(req.piExecutable).Run(runCtx, pi.WorkerRequest{
		Model:          req.model,
		ThinkingLevel:  req.thinkingLevel,
		Prompt:         req.prompt,
		Workspace:      req.workspace,
		WorkerID:       req.workerID,
		OnProcessStart: func(workerID, pid int) { notify(workerID, pid) },
	})

	// If every notification reached the wire intact, exactly one
	// terminal result frame follows. A notification failure leaves the
	// stream broken — appending the terminal frame after a partial
	// notification frame could corrupt what the parent already reads —
	// so no terminal frame is appended then: the host returns the
	// notification failure and exits, and the parent's drain classifies
	// the outcome. A run cancelled or timed out through the context
	// whose notifications all reached the wire still reports its
	// terminal result normally; the worker result is preserved field
	// for field, and the strict parent-side decode is what refuses an
	// invalid terminal result, and an invalid terminal result never
	// implies success.
	if notifyErr != nil {
		return workerHostExchange{request: req, result: workerResult}, notifyErr
	}
	terminalPayload, encErr := encodeWorkerHostResult(workerResult)
	if encErr != nil {
		return workerHostExchange{request: req, result: workerResult}, encErr
	}
	if writeErr := writeWorkerHostResponse(pipes.responseWriter, terminalPayload); writeErr != nil {
		return workerHostExchange{request: req, result: workerResult}, errors.Join(
			fmt.Errorf("receive worker host: write terminal result frame: %w", writeErr),
			notifyErr)
	}
	return workerHostExchange{request: req, result: workerResult}, notifyErr
}

// watchWorkerHostOwnership watches the child ownership descriptor (fd 5)
// until it reaches EOF, fails, or the exchange stops it, then cancels
// the host context on owner loss. The ownership pipe carries no bytes:
// a byte on it is a protocol violation, and a read failure means the
// ownership channel is gone — all three are treated as owner loss, the
// conservative outcome. The ownership descriptor is a blocking file
// that closing cannot wake, so the watcher polls the raw descriptor in
// bounded slices and checks the stop channel between slices instead of
// blocking in a read forever; it only ever touches the raw descriptor,
// never the os.File wrapper, so it cannot race the exchange's pipe
// close. On owner loss the watcher cancels the context and then invokes
// expireRequestRead, which expires the request-read deadline so a
// pending partial request read returns instead of pinning the exchange.
func watchWorkerHostOwnership(fd int, cancel context.CancelFunc, expireRequestRead func(), stopped <-chan struct{}) {
	pollFds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	buf := make([]byte, 1)
	for {
		n, pollErr := unix.Poll(pollFds, 100)
		if pollErr == unix.EINTR {
			continue
		}
		select {
		case <-stopped:
			return
		default:
		}
		if n > 0 {
			// Readable: data, EOF, or hangup. Whatever the read yields,
			// ownership is gone.
			_, _ = unix.Read(fd, buf)
			cancel()
			expireRequestRead()
			return
		}
		if n < 0 && pollErr != nil {
			// Polling itself is broken: treat ownership as lost, the
			// conservative outcome.
			cancel()
			expireRequestRead()
			return
		}
	}
}

// writeWorkerHostResponse writes one bounded response frame under a real
// absolute write deadline: the child host's response descriptor is
// pollable (openChildRolePipes), so the runtime poller fails a write
// that cannot complete within the grace instead of letting it pin the
// host's cleanup or exit. A failure to arm the deadline is reported
// honestly — an un-deadlined write could block forever, so it is never
// attempted silently.
func writeWorkerHostResponse(w io.Writer, payload []byte) error {
	if f, ok := w.(interface{ SetWriteDeadline(time.Time) error }); ok {
		if err := f.SetWriteDeadline(time.Now().Add(workerHostWriteGrace)); err != nil {
			return fmt.Errorf("set write deadline: %w", err)
		}
	}
	return writeFrame(w, payload, privateFrameLimit)
}

// execute launches one private same-binary worker-host child, sends the
// encoded execution request, closes the request writer (request EOF does
// not cancel the host), and drives the framed exchange to one terminal
// result. Cancellation closes ownership first and drains the host's own
// cleanup; the deferred Close is reached only after the host is reaped,
// so it never kills a host that may still be cleaning Pi. Only the
// bounded fallback kill ends a host early, and the result then states
// the cleanup uncertainty explicitly.
func (a *workerHostAdapter) execute(ctx context.Context, req pi.WorkerRequest, payload []byte) (result pi.WorkerResult) {
	proc, startErr := startRoleProcess(a.executable, roleWorkerHost)
	if startErr != nil {
		return pi.WorkerResult{Model: req.Model, Status: pi.StatusUnavailable, Error: fmt.Sprintf("start worker host: %v", startErr)}
	}
	defer func() { _ = proc.Close() }()

	// Phase 1 — send exactly one request frame. The send runs on a
	// worker goroutine so a host that never reads cannot stall
	// cancellation.
	sendDone := make(chan error, 1)
	go func() { sendDone <- proc.Send(payload) }()
	select {
	case <-ctx.Done():
		// The caller cancelled or timed out while the request was in
		// flight. Close ownership first — never Close, which would kill
		// the host before it could clean any Pi it already runs — then
		// wait (bounded) for the host to exit on its own.
		_ = proc.CloseOwnership()
		origin := hostDrainOriginFromCtx(ctx)
		bound := workerHostCleanupBound
		select {
		case <-sendDone:
		case <-time.After(bound):
		}
		killed, waitErr := reapWorkerHost(proc, bound)
		return workerHostNoTerminalResult(req.Model, origin, bound, killed, waitErr != nil && !killed)
	case sendErr := <-sendDone:
		if sendErr != nil {
			// The request never reached the host intact, so no Pi can
			// run under it: close ownership and reap it, killing only if
			// it lingers beyond the bound.
			_ = proc.CloseOwnership()
			origin := hostDrainOrigin{status: pi.StatusError, head: "worker failure",
				cause: fmt.Errorf("send worker host request frame: %w", sendErr)}
			bound := workerHostCleanupBound
			killed, waitErr := reapWorkerHost(proc, bound)
			return workerHostNoTerminalResult(req.Model, origin, bound, killed, waitErr != nil && !killed)
		}
	}
	// The complete request is on the wire. Close the request writer: the
	// request EOF does not cancel the host — only ownership EOF does —
	// and simply finishes the one-request protocol.
	_ = proc.CloseRequest()

	// Phase 2 — read response frames until the one terminal result.
	// Each receive runs on its own worker goroutine; the main goroutine
	// observes ctx.Done between frames and performs every terminal
	// action, so CloseOwnership never races a receive. Exactly one
	// receive is in flight at any select, and every bail path drains it
	// before returning, so no receive goroutine outlives Run.
	type receiveOutcome struct {
		frame []byte
		err   error
	}
	recvDone := make(chan receiveOutcome, 1)
	nextReceive := func() {
		go func() {
			frame, recvErr := proc.Receive()
			recvDone <- receiveOutcome{frame: frame, err: recvErr}
		}()
	}
	nextReceive()

	var (
		ctxDone    <-chan struct{} = ctx.Done()
		drainTimer <-chan time.Time
		bound      = workerHostCleanupBound
		draining   bool
		// protocolFailure is set when the drain began because the host
		// produced a malformed or unknown frame: the host is
		// untrustworthy, and no terminal frame it sends afterwards may
		// override the protocol-failure drain outcome.
		protocolFailure bool
		origin          hostDrainOrigin
		terminal        *pi.WorkerResult
	)
	for terminal == nil {
		select {
		case out := <-recvDone:
			switch {
			case out.err != nil:
				// The response stream ended or broke before the terminal
				// result. The host is gone or unreachable: no terminal
				// frame will arrive.
				if !draining {
					draining = true
					origin = hostDrainOrigin{status: pi.StatusError, head: "worker failure",
						cause: fmt.Errorf("read worker host response: %w", out.err)}
					_ = proc.CloseOwnership()
					drainTimer = time.After(bound)
				}
				killed, waitErr := reapWorkerHost(proc, bound)
				return workerHostNoTerminalResult(req.Model, origin, bound, killed, waitErr != nil && !killed)
			default:
				frame, decodeErr := decodeWorkerHostResponse(out.frame)
				if decodeErr != nil {
					// A malformed or unknown frame is a protocol failure.
					// The host is untrustworthy but may still run Pi, so
					// close ownership and drain (bounded) for a possible
					// terminal or EOF. The drain origin classifies the
					// outcome as non-success: no terminal frame the host
					// sends after a malformed one can override it.
					if !draining {
						draining = true
						protocolFailure = true
						origin = hostDrainOrigin{status: pi.StatusError, head: "worker failure",
							cause: fmt.Errorf("decode worker host response frame: %w", decodeErr)}
						_ = proc.CloseOwnership()
						drainTimer = time.After(bound)
						nextReceive()
						continue
					}
					// A second malformed frame while draining: nothing
					// more can be learned from this host.
					killed, waitErr := reapWorkerHost(proc, bound)
					return workerHostNoTerminalResult(req.Model, origin, bound, killed, waitErr != nil && !killed)
				}
				switch frame.kind {
				case workerHostFrameProcessStart:
					if req.OnProcessStart != nil {
						req.OnProcessStart(frame.workerID, frame.pid)
					}
					nextReceive()
				case workerHostFrameResult:
					if protocolFailure {
						// The stream already produced a malformed or
						// unknown frame, so this host's later terminal
						// frame — even a genuine completed one — cannot
						// override the protocol failure: the drain
						// continues until the host exits or the cleanup
						// bound ends it, and the outcome stays the
						// non-success drain result. A genuine valid
						// terminal arriving on an intact stream (no
						// protocol failure) is untouched below: a drain
						// that began with cancellation or timeout still
						// returns the host's own terminal result.
						nextReceive()
						continue
					}
					terminal = &frame.result
				}
			}
		case <-ctxDone:
			// Cancellation or timeout while the exchange is in flight.
			ctxDone = nil
			if !draining {
				draining = true
				origin = hostDrainOriginFromCtx(ctx)
			}
			if errors.Is(ctx.Err(), context.Canceled) {
				// Cancellation closes ownership first and drains: the
				// host finishes its Pi cleanup, reports its terminal
				// result, and exits on its own.
				_ = proc.CloseOwnership()
			}
			// DeadlineExceeded leaves ownership open: the child host
			// enforces the same execution deadline itself, and its own
			// timeout — not an ownership cancellation — classifies the
			// outcome as timed out.
			drainTimer = time.After(bound)
		case <-drainTimer:
			// Bounded fallback: the host did not settle within the
			// bound. Kill and reap it, then drain the in-flight receive:
			// Kill reaps the host first, so its response writer is closed
			// and the blocked read returns EOF. The cleanup uncertainty is
			// reported explicitly.
			if !draining {
				draining = true
				origin = hostDrainOrigin{status: pi.StatusError, head: "worker failure",
					cause: errors.New("worker host did not settle within the cleanup bound")}
			}
			_ = proc.Kill()
			<-recvDone
			return workerHostNoTerminalResult(req.Model, origin, bound, true, false)
		}
	}

	// A terminal result frame is authoritative: the host writes it only
	// after pi.Worker.Run returned, whose deferred Close already finished
	// the Pi containment cleanup. Reap the host within the bound; only a
	// host stuck after its terminal frame is killed, and the received
	// result stands untouched.
	_, _ = reapWorkerHost(proc, bound)
	return *terminal
}

// hostDrainOrigin describes why a drain began, so the synthesized result
// for a host that ended without a terminal frame classifies the outcome
// from the drain's own cause rather than from whatever fired later.
type hostDrainOrigin struct {
	status string
	head   string
	cause  error
}

// hostDrainOriginFromCtx classifies a drain that began because the
// caller context ended, mirroring the worker's own context
// classification: an expired deadline is a timeout, a cancellation is a
// cancellation.
func hostDrainOriginFromCtx(ctx context.Context) hostDrainOrigin {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return hostDrainOrigin{status: pi.StatusTimedOut, head: "timed out", cause: ctx.Err()}
	}
	return hostDrainOrigin{status: pi.StatusCancelled, head: "cancelled", cause: ctx.Err()}
}

// workerHostNoTerminalResult assembles the honest result when the host
// ended without a terminal result frame: the drain origin's status, the
// cause, and an explicit statement of what is and is not known about Pi
// cleanup. A host that was force-killed or that exited abnormally leaves
// its Pi cleanup uncertain; only a host that exited cleanly on its own
// after ownership loss is treated as having finished its own cleanup.
// This result is never a success.
func workerHostNoTerminalResult(model string, origin hostDrainOrigin, bound time.Duration, killed, crashed bool) pi.WorkerResult {
	msg := fmt.Sprintf("%s: %v", origin.head, origin.cause)
	switch {
	case killed:
		msg += fmt.Sprintf("; worker host was force-killed after %s before reporting a terminal result; its Pi cleanup outcome is uncertain", bound)
	case crashed:
		msg += "; worker host exited abnormally before reporting a terminal result; its Pi cleanup outcome is uncertain"
	default:
		msg += "; worker host exited before reporting a terminal result"
	}
	return pi.WorkerResult{Model: model, Status: origin.status, Error: msg}
}

// reapWorkerHost waits up to bound for the host process to exit and
// reports whether the bounded fallback kill was required and the wait
// error (nil for a clean exit). When the fallback fires, Kill reaps the
// host before reapWorkerHost returns, so a concurrent Wait goroutine
// always finishes.
func reapWorkerHost(proc *roleProcess, bound time.Duration) (forceKilled bool, waitErr error) {
	waitDone := make(chan error, 1)
	go func() { waitDone <- proc.Wait() }()
	select {
	case waitErr = <-waitDone:
		return false, waitErr
	case <-time.After(bound):
		_ = proc.Kill()
		return true, <-waitDone
	}
}

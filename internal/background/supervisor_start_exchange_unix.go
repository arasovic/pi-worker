//go:build darwin || linux

package background

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// supervisorStartPrepareFunc is the concrete preparation function used by
// the child-side supervisor start handshake. Production always passes
// prepareSupervisorStart; the parameter exists only so tests can inject a
// stub that returns a non-nil preparation together with an error, the
// retry-only cleanup branch that production preparation can reach only
// when a Store.Create failure is combined with a failing ticket cancel.
type supervisorStartPrepareFunc func(req supervisorStartRequest) (*supervisorPreparation, error)

// supervisorStartResult is the outcome of one child-side supervisor start
// exchange over the child role pipes.
//
// accepted reports whether the complete accepted reply frame reached the
// wire. request is populated only for accepted results; preparation is
// populated for accepted results and may also be present on a rejected
// result whose rollback cleanup stayed incomplete. Callers must check
// accepted before interpreting the returned error: an accepted result may
// still carry a pipe-close diagnostic, and it proceeds to worker
// scheduling regardless. A rejected result that carries the preparation
// with accepted false must never be scheduled; the caller owns it and
// must finish the rollback.
type supervisorStartResult struct {
	request     supervisorStartRequest
	preparation *supervisorPreparation
	accepted    bool
}

// receiveSupervisorStart runs the production child-side supervisor start
// handshake over pipes. It takes ownership of the supervisor role
// transport ends — the request reader on fd 3 and the response writer on
// fd 4 — and closes both on every return; the worker-host ownership
// reader on fd 5 is not part of this contract. It reads exactly one
// bounded frame and decodes one strict start request, durably prepares
// the accepted Snapshot and admission tickets through
// prepareSupervisorStart, and writes exactly one accepted or rejected
// reply frame. It never waits for a second frame or starter EOF, and it
// never reads again once the first request frame has been consumed.
func receiveSupervisorStart(pipes *childRolePipes) (supervisorStartResult, error) {
	return receiveSupervisorStartWithPrepare(pipes, prepareSupervisorStart)
}

// receiveSupervisorStartWithPrepare is receiveSupervisorStart with the
// preparation function injected; production passes prepareSupervisorStart,
// and tests substitute a concrete stub to exercise the otherwise
// unreachable preparation-plus-error cleanup branch.
func receiveSupervisorStartWithPrepare(pipes *childRolePipes, prepare supervisorStartPrepareFunc) (result supervisorStartResult, err error) {
	// Every return closes both supervisor role transport ends. Named
	// returns let a pipe-close failure join the returned error without
	// disturbing an already-decided result: an accepted result keeps
	// accepted true and is never rolled back because of a close error.
	defer func() {
		if closeErr := closeSupervisorStartPipes(pipes); closeErr != nil {
			err = joinSupervisorStartErrors(err, closeErr)
		}
	}()

	if pipes == nil {
		return supervisorStartResult{}, fmt.Errorf("receive supervisor start: pipes must not be nil")
	}

	// Read exactly one bounded request frame. There is no EOF or second
	// frame loop: a single readFrame either yields the one request or
	// fails, and the handshake answers either way.
	payload, readErr := readFrame(pipes.requestReader, privateFrameLimit)
	if readErr != nil {
		// The request never arrived intact. Answer the handshake with a
		// best-effort rejection so a waiting starter still receives a
		// complete reply, and report the transport failure.
		wrapped := fmt.Errorf("receive supervisor start: read request frame: %w", readErr)
		return supervisorStartResult{}, joinSupervisorStartErrors(wrapped, rejectSupervisorStart(pipes.responseWriter, wrapped))
	}

	// Decode one strict start request. A frame that is not a valid
	// request is a protocol-level rejection: once the complete rejection
	// frame is on the wire and nothing was created, the exchange ended
	// cleanly and there is nothing left to report. A rejection write
	// failure joins the decode cause instead of replacing it.
	req, decodeErr := decodeSupervisorStartRequest(payload)
	if decodeErr != nil {
		if rejectErr := rejectSupervisorStart(pipes.responseWriter, decodeErr); rejectErr != nil {
			return supervisorStartResult{}, joinSupervisorStartErrors(decodeErr, rejectErr)
		}
		return supervisorStartResult{}, nil
	}

	// Prepare the accepted Snapshot and tickets. Acceptance never happens
	// before this preparation is durable.
	prep, prepErr := prepare(req)
	if prepErr != nil {
		if prep == nil {
			// Nothing was persisted and nothing needs cleanup: a clean
			// rejection. A rejection write failure joins the prepare
			// cause instead of replacing it.
			if rejectErr := rejectSupervisorStart(pipes.responseWriter, prepErr); rejectErr != nil {
				return supervisorStartResult{}, joinSupervisorStartErrors(prepErr, rejectErr)
			}
			return supervisorStartResult{}, nil
		}

		// The preparation came back holding durable state that its own
		// cleanup could not finish: roll it back exactly once before
		// rejection. rollback is retryable until it returns nil, but the
		// handshake never loops or retries: an attempt that stays
		// incomplete preserves the preparation on the result, so the
		// caller — and only the caller — can finish the rollback.
		// accepted stays false, so it is never scheduled.
		cleanupErrs := []error{prepErr}
		if rollbackErr := prep.rollback(); rollbackErr != nil {
			cleanupErrs = append(cleanupErrs, rollbackErr)
			result.preparation = prep
		}

		// Reject with the full history; a rejection write failure joins
		// under it instead of replacing it.
		joined := joinSupervisorStartErrors(cleanupErrs...)
		if rejectErr := rejectSupervisorStart(pipes.responseWriter, joined); rejectErr != nil {
			return result, joinSupervisorStartErrors(joined, rejectErr)
		}

		// Cleanup completed: a clean rejection — nil error, no preserved
		// preparation, the failure living only in the rejection reason.
		// Cleanup incomplete: return the joined history together with the
		// preserved preparation, which the caller must finish rolling
		// back.
		if result.preparation != nil {
			return result, joined
		}
		return supervisorStartResult{}, nil
	}
	if prep == nil {
		// Production preparation never succeeds without a preparation;
		// treat the contract violation as a rejection and report it.
		violation := fmt.Errorf("receive supervisor start: prepare returned no preparation")
		if rejectErr := rejectSupervisorStart(pipes.responseWriter, violation); rejectErr != nil {
			return supervisorStartResult{}, joinSupervisorStartErrors(violation, rejectErr)
		}
		return supervisorStartResult{}, violation
	}

	// Encode and write exactly one accepted reply frame. Encoding happens
	// before the first byte reaches the wire, so an encoding failure is a
	// zero-byte failure.
	acceptedPayload, encodeErr := encodeSupervisorStartAccepted(prep.snapshot)
	if encodeErr != nil {
		wrapped := fmt.Errorf("receive supervisor start: encode accepted reply frame: %w", encodeErr)
		return failSupervisorStartAccepted(prep, pipes.responseWriter, wrapped, 0)
	}

	// The counting writer records how many accepted-frame bytes the pipe
	// actually consumed, distinguishing a zero-byte write failure — before
	// any accepted-frame byte, so a rejection may still be attempted —
	// from a partial accepted frame, which must never be followed by a
	// rejection.
	counter := &supervisorStartByteCounter{w: pipes.responseWriter}
	if writeErr := writeFrame(counter, acceptedPayload, privateFrameLimit); writeErr != nil {
		wrapped := fmt.Errorf("receive supervisor start: write accepted reply frame: %w", writeErr)
		return failSupervisorStartAccepted(prep, pipes.responseWriter, wrapped, counter.n)
	}

	// The complete accepted frame is on the wire: acceptance is now
	// irreversible. A later pipe-close failure is joined into err by the
	// deferred close while result keeps accepted true and is never rolled
	// back; request and preparation are the complete decoded request and
	// the durable preparation for worker scheduling.
	result = supervisorStartResult{request: req, preparation: prep, accepted: true}
	return result, nil
}

// failSupervisorStartAccepted handles an accepted-reply encoding or write
// failure: the accepted frame did not fully reach the wire, so the
// unconfirmed preparation is rolled back and the outcome stays
// non-accepted. bytesWritten counts the accepted-frame bytes already
// consumed by the response writer: zero allows one rejection frame before
// any accepted-frame byte was written, while any partial accepted frame
// forbids a rejection. A rollback that stays incomplete leaves the
// preparation on the result so the caller can retry it; accepted is
// always false here, so the preparation is never scheduled. All failures
// are preserved with errors.Join, and nothing here retries.
func failSupervisorStartAccepted(prep *supervisorPreparation, responseWriter io.Writer, cause error, bytesWritten int64) (supervisorStartResult, error) {
	var result supervisorStartResult

	rollbackErr := prep.rollback()
	errs := []error{cause}
	if rollbackErr != nil {
		errs = append(errs, rollbackErr)
		result.preparation = prep
	}

	if bytesWritten == 0 {
		// No accepted-frame byte reached the wire, so a rejection is still
		// a complete, unambiguous answer to the handshake.
		if rejectErr := rejectSupervisorStart(responseWriter, joinSupervisorStartErrors(errs...)); rejectErr != nil {
			errs = append(errs, rejectErr)
		}
	}

	return result, joinSupervisorStartErrors(errs...)
}

// rejectSupervisorStart encodes and writes exactly one complete rejection
// reply frame whose reason describes cause. A nil or unusable cause falls
// back to a fixed non-blank reason, so the frame is always encodable.
func rejectSupervisorStart(w io.Writer, cause error) error {
	payload, err := encodeSupervisorStartRejected(supervisorStartRejectionReason(cause))
	if err != nil {
		return fmt.Errorf("receive supervisor start: encode rejection frame: %w", err)
	}
	if err := writeFrame(w, payload, privateFrameLimit); err != nil {
		return fmt.Errorf("receive supervisor start: write rejection frame: %w", err)
	}
	return nil
}

// supervisorStartRejectionReason converts a failure into the reason text
// for a rejection frame. Rejection reasons must be non-blank valid UTF-8;
// anything unusable falls back to a fixed reason.
func supervisorStartRejectionReason(cause error) string {
	const fallback = "supervisor start rejected"
	if cause == nil {
		return fallback
	}
	reason := cause.Error()
	if reason == "" || !utf8.ValidString(reason) || strings.TrimSpace(reason) == "" {
		return fallback
	}
	return reason
}

// closeSupervisorStartPipes closes the two supervisor role transport ends
// owned by a supervisor start exchange — the request reader on fd 3 and
// the response writer on fd 4 — mirroring childRolePipes.Close for those
// two ends. It is nil-safe. The worker-host ownership reader on fd 5 is
// deliberately left untouched: fd 5 is not part of this contract.
func closeSupervisorStartPipes(pipes *childRolePipes) error {
	if pipes == nil {
		return nil
	}
	var closeErrs []error

	if pipes.requestReader != nil {
		if err := pipes.requestReader.Close(); err != nil {
			closeErrs = append(closeErrs, fmt.Errorf("close request fd: %w", err))
		}
		pipes.requestReader = nil
	}

	if pipes.responseWriter != nil {
		if err := pipes.responseWriter.Close(); err != nil {
			closeErrs = append(closeErrs, fmt.Errorf("close response fd: %w", err))
		}
		pipes.responseWriter = nil
	}

	if len(closeErrs) == 1 {
		return closeErrs[0]
	}
	return errors.Join(closeErrs...)
}

// joinSupervisorStartErrors joins non-nil errors in order. It returns nil
// when nothing failed and the single error itself when only one is
// present, so error identity survives single-failure returns; multiple
// failures are preserved with errors.Join.
func joinSupervisorStartErrors(errs ...error) error {
	var nonNil []error
	for _, err := range errs {
		if err != nil {
			nonNil = append(nonNil, err)
		}
	}
	switch len(nonNil) {
	case 0:
		return nil
	case 1:
		return nonNil[0]
	default:
		return errors.Join(nonNil...)
	}
}

// supervisorStartByteCounter is a tiny counting writer around the
// response writer: it forwards every Write and records the total number
// of bytes the underlying writer accepted, so a failed accepted-frame
// write can be classified as zero-byte (nothing accepted) or partial
// (some accepted-frame bytes reached the wire).
type supervisorStartByteCounter struct {
	w io.Writer
	n int64
}

// Write forwards p to the underlying writer and accumulates the count of
// accepted bytes.
func (c *supervisorStartByteCounter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

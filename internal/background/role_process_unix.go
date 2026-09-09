//go:build darwin || linux

package background

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
)

// errRoleRequestClosed reports that the parent has closed its request
// writer, so no further requests can be sent.
var errRoleRequestClosed = errors.New("role process request channel closed")

// roleProcess owns a spawned child process and the pipe ends the parent
// uses to exchange framed requests and responses with it. The child
// inherits the request reader as fd 3 and the response writer as fd 4.
// For roleWorkerHost the child also receives the ownership read end on fd 5,
// and the parent retains the ownership write end on the ownershipWriter field.
type roleProcess struct {
	cmd  *exec.Cmd
	role role // validated role from startRoleProcess: roleSupervisor or roleWorkerHost

	requestWriter  *os.File // parent-side write end of the request pipe
	responseReader *os.File // parent-side read end of the response pipe
	frameLimit     int      // limit for Send/Receive frames (0 means use default)

	// sendMu and receiveMu serialize parent-side frame I/O in each
	// direction independently.
	sendMu    sync.Mutex
	receiveMu sync.Mutex

	// requestCloseMu guards the idempotent close of the request writer
	// and the cached close error.
	requestCloseMu  sync.Mutex
	requestClosed   bool
	requestCloseErr error

	// waitOnce guarantees cmd.Wait is called exactly once; all concurrent
	// callers of Wait receive the same cached error.
	waitOnce sync.Once
	done     chan struct{}
	waitErr  error

	// killProcess is the process-kill seam. It is nil in production, where
	// Kill signals cmd.Process directly; tests set it per instance to force
	// a deterministic kill failure.
	killProcess func() error

	// respCloseMu guards the idempotent close of the response reader.
	respCloseMu  sync.Mutex
	respClosed   bool
	respCloseErr error

	// ownershipCloseMu guards the idempotent close of the ownership writer.
	ownershipCloseMu  sync.Mutex
	ownershipClosed   bool
	ownershipCloseErr error

	// closeOnce guarantees the terminal lifecycle shared by Close and
	// Detach performs its full cleanup exactly once: whichever of the
	// two is called first wins, and concurrent or later callers of
	// either block until it finishes and then receive the same cached
	// result.
	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error

	// ownershipWriter is the parent-side write end of the ownership pipe.
	// It is set only for roleWorkerHost and nil for roleSupervisor.
	ownershipWriter *os.File
}

// startRoleProcess validates the executable and role before creating any
// pipes, creates the request and response pipe pairs, and starts the child
// with the request reader and response writer installed as fds 3 and 4 via
// ExtraFiles. The child inherits the parent environment; stdin, stdout,
// and stderr stay nil so exec connects them to the null device. On a
// failed Start all four pipe ends are closed and their close errors are
// joined with the start error. After a successful Start the child-side
// copies held by the parent are closed immediately; if either close fails
// the parent ends are closed too, the exact child is killed, and Wait
// reaps it so no child survives.
func startRoleProcess(executable string, r role) (*roleProcess, error) {
	if executable == "" {
		return nil, fmt.Errorf("executable must not be empty")
	}
	if !validRole(r) {
		return nil, fmt.Errorf("invalid role %q", r)
	}

	requestReader, requestWriter, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create request pipe: %w", err)
	}
	responseReader, responseWriter, err := os.Pipe()
	if err != nil {
		closeErrs := []error{fmt.Errorf("create response pipe: %w", err)}
		if cErr := requestReader.Close(); cErr != nil {
			closeErrs = append(closeErrs, fmt.Errorf("close request reader: %w", cErr))
		}
		if cErr := requestWriter.Close(); cErr != nil {
			closeErrs = append(closeErrs, fmt.Errorf("close request writer: %w", cErr))
		}
		if len(closeErrs) == 1 {
			return nil, closeErrs[0]
		}
		return nil, errors.Join(closeErrs...)
	}

	var ownershipReader, ownershipWriter *os.File
	if r == roleWorkerHost {
		ownershipReader, ownershipWriter, err = os.Pipe()
		if err != nil {
			closeErrs := []error{fmt.Errorf("create ownership pipe: %w", err)}
			if cErr := requestReader.Close(); cErr != nil {
				closeErrs = append(closeErrs, fmt.Errorf("close request reader: %w", cErr))
			}
			if cErr := requestWriter.Close(); cErr != nil {
				closeErrs = append(closeErrs, fmt.Errorf("close request writer: %w", cErr))
			}
			if cErr := responseReader.Close(); cErr != nil {
				closeErrs = append(closeErrs, fmt.Errorf("close response reader: %w", cErr))
			}
			if cErr := responseWriter.Close(); cErr != nil {
				closeErrs = append(closeErrs, fmt.Errorf("close response writer: %w", cErr))
			}
			if len(closeErrs) == 1 {
				return nil, closeErrs[0]
			}
			return nil, errors.Join(closeErrs...)
		}
	}

	cmd := exec.Command(executable, string(r))
	if r == roleWorkerHost {
		cmd.ExtraFiles = []*os.File{requestReader, responseWriter, ownershipReader}
	} else {
		cmd.ExtraFiles = []*os.File{requestReader, responseWriter}
	}

	if err := cmd.Start(); err != nil {
		closeErrs := []error{fmt.Errorf("start role process: %w", err)}
		if cErr := requestReader.Close(); cErr != nil {
			closeErrs = append(closeErrs, fmt.Errorf("close request reader: %w", cErr))
		}
		if cErr := requestWriter.Close(); cErr != nil {
			closeErrs = append(closeErrs, fmt.Errorf("close request writer: %w", cErr))
		}
		if cErr := responseReader.Close(); cErr != nil {
			closeErrs = append(closeErrs, fmt.Errorf("close response reader: %w", cErr))
		}
		if cErr := responseWriter.Close(); cErr != nil {
			closeErrs = append(closeErrs, fmt.Errorf("close response writer: %w", cErr))
		}
		if ownershipReader != nil {
			if cErr := ownershipReader.Close(); cErr != nil {
				closeErrs = append(closeErrs, fmt.Errorf("close ownership reader: %w", cErr))
			}
		}
		if ownershipWriter != nil {
			if cErr := ownershipWriter.Close(); cErr != nil {
				closeErrs = append(closeErrs, fmt.Errorf("close ownership writer: %w", cErr))
			}
		}
		if len(closeErrs) == 1 {
			return nil, closeErrs[0]
		}
		return nil, errors.Join(closeErrs...)
	}

	// The child now owns its copies of the request reader, response
	// writer, and (for worker-host) ownership reader; close the parent's
	// copies so the pipe ends observe EOF as soon as the child exits.
	closeErrs := make([]error, 0, 4)
	if err := requestReader.Close(); err != nil {
		closeErrs = append(closeErrs, fmt.Errorf("close child request reader: %w", err))
	}
	if err := responseWriter.Close(); err != nil {
		closeErrs = append(closeErrs, fmt.Errorf("close child response writer: %w", err))
	}
	if ownershipReader != nil {
		if err := ownershipReader.Close(); err != nil {
			closeErrs = append(closeErrs, fmt.Errorf("close child ownership reader: %w", err))
		}
	}
	if len(closeErrs) > 0 {
		// A surviving child would keep the pipes open forever;
		// close every parent end, kill the exact child, and reap it.
		if err := requestWriter.Close(); err != nil {
			closeErrs = append(closeErrs, fmt.Errorf("close request writer: %w", err))
		}
		if err := responseReader.Close(); err != nil {
			closeErrs = append(closeErrs, fmt.Errorf("close response reader: %w", err))
		}
		if ownershipWriter != nil {
			if err := ownershipWriter.Close(); err != nil {
				closeErrs = append(closeErrs, fmt.Errorf("close ownership writer: %w", err))
			}
		}
		if err := cmd.Process.Kill(); err != nil {
			closeErrs = append(closeErrs, fmt.Errorf("kill role process: %w", err))
		}
		if err := cmd.Wait(); err != nil {
			closeErrs = append(closeErrs, fmt.Errorf("wait role process: %w", err))
		}
		return nil, errors.Join(closeErrs...)
	}

	rp := &roleProcess{
		cmd:            cmd,
		role:           r,
		requestWriter:  requestWriter,
		responseReader: responseReader,
		frameLimit:     privateFrameLimit,
		done:           make(chan struct{}),
		closeDone:      make(chan struct{}),
	}
	// Store ownershipWriter only for worker-host; supervisor gets nil.
	if ownershipWriter != nil {
		rp.ownershipWriter = ownershipWriter
	}
	return rp, nil
}

// Send writes one request frame to the child under sendMu. It rejects a
// nil process and a request writer that has already been closed.

// effectiveFrameLimit returns p.frameLimit when positive, otherwise
// falls back to the default frame limit.
func (p *roleProcess) effectiveFrameLimit() int {
	if p.frameLimit > 0 {
		return p.frameLimit
	}
	return privateFrameLimit
}

func (p *roleProcess) Send(payload []byte) error {
	if p == nil {
		return fmt.Errorf("send on nil role process")
	}
	p.sendMu.Lock()
	defer p.sendMu.Unlock()

	p.requestCloseMu.Lock()
	closed := p.requestClosed
	closeErr := p.requestCloseErr
	p.requestCloseMu.Unlock()
	if closed {
		if closeErr != nil {
			return errors.Join(errRoleRequestClosed, closeErr)
		}
		return errRoleRequestClosed
	}
	if p.requestWriter == nil {
		return fmt.Errorf("role process request writer is nil")
	}
	return writeFrame(p.requestWriter, payload, p.effectiveFrameLimit())
}

// Receive reads one response frame from the child under receiveMu. It
// rejects a nil process.
func (p *roleProcess) Receive() ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("receive on nil role process")
	}
	p.receiveMu.Lock()
	defer p.receiveMu.Unlock()

	if p.responseReader == nil {
		return nil, fmt.Errorf("role process response reader is nil")
	}
	return readFrame(p.responseReader, p.effectiveFrameLimit())
}

// CloseRequest closes the parent request writer exactly once. It acquires
// sendMu before requestCloseMu to match Send's lock order, holding sendMu
// until the writer close completes. It is nil-safe and idempotent, and
// caches the close error for later callers.
func (p *roleProcess) CloseRequest() error {
	if p == nil {
		return nil
	}
	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	return p.closeRequestWriter()
}

// closeRequestWriter closes the parent request writer exactly once under
// requestCloseMu only, without taking sendMu. It is the shared body of
// CloseRequest and of the failed-kill branch of Close, which must not wait
// for a Send that is blocked writing to a child the kill did not remove.
func (p *roleProcess) closeRequestWriter() error {
	p.requestCloseMu.Lock()
	defer p.requestCloseMu.Unlock()

	if p.requestClosed {
		return p.requestCloseErr
	}
	p.requestClosed = true
	if p.requestWriter == nil {
		return nil
	}
	p.requestCloseErr = p.requestWriter.Close()
	if p.requestCloseErr != nil {
		p.requestCloseErr = fmt.Errorf("close role process request writer: %w", p.requestCloseErr)
	}
	return p.requestCloseErr
}

// Wait waits for the child process to exit. It is nil-safe and calls
// cmd.Wait exactly once via sync.Once; all concurrent callers block until
// cmd.Wait returns and receive the same cached error.
func (p *roleProcess) Wait() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	p.waitOnce.Do(func() {
		defer close(p.done)
		p.waitErr = p.cmd.Wait()
	})
	<-p.done
	return p.waitErr
}

// closeResponse closes the parent response reader exactly once. It
// serializes with Receive via receiveMu, is nil-safe and idempotent,
// and caches the wrapped close error.
func (p *roleProcess) closeResponse() error {
	if p == nil {
		return nil
	}
	p.receiveMu.Lock()
	defer p.receiveMu.Unlock()
	return p.closeResponseReader()
}

// closeResponseReader closes the parent response reader exactly once under
// respCloseMu only, without taking receiveMu. It is the shared body of
// closeResponse and of the failed-kill branch of Close, which must not wait
// for a Receive that is blocked reading from a child the kill did not
// remove.
func (p *roleProcess) closeResponseReader() error {
	p.respCloseMu.Lock()
	defer p.respCloseMu.Unlock()

	if p.respClosed {
		return p.respCloseErr
	}
	p.respClosed = true
	if p.responseReader == nil {
		return nil
	}
	p.respCloseErr = p.responseReader.Close()
	if p.respCloseErr != nil {
		p.respCloseErr = fmt.Errorf("close role process response reader: %w", p.respCloseErr)
	}
	return p.respCloseErr
}

// killChildProcess signals the child through the per-instance seam when one
// is installed, and directly otherwise.
func (p *roleProcess) killChildProcess() error {
	if p.killProcess != nil {
		return p.killProcess()
	}
	return p.cmd.Process.Kill()
}

// Kill terminates the child process. It is nil-safe and idempotent:
// if the process is already gone it returns nil; otherwise it kills
// cmd.Process, always calls Wait to reap the zombie, and returns any
// unexpected Kill infrastructure errors. A normal non-zero exit from
// Wait is treated as a child outcome, not a cleanup failure.
func (p *roleProcess) Kill() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	err := p.killChildProcess()
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("kill role process: %w", err)
	}
	// Reap the process regardless of whether Kill succeeded.
	_ = p.Wait()
	return nil
}

// CloseOwnership closes the parent ownership writer exactly once. It is
// nil-safe and idempotent, caches the close error, and writes no bytes to
// the pipe. For supervisors (which never received an ownership pipe) it
// returns nil immediately.
func (p *roleProcess) CloseOwnership() error {
	if p == nil {
		return nil
	}
	p.ownershipCloseMu.Lock()
	defer p.ownershipCloseMu.Unlock()

	if p.ownershipClosed {
		return p.ownershipCloseErr
	}
	p.ownershipClosed = true
	if p.ownershipWriter == nil {
		return nil
	}
	p.ownershipCloseErr = p.ownershipWriter.Close()
	if p.ownershipCloseErr != nil {
		p.ownershipCloseErr = fmt.Errorf("close role process ownership writer: %w", p.ownershipCloseErr)
	}
	return p.ownershipCloseErr
}

// Close performs a full lifecycle teardown: Kill/reap the exact child
// first (to unblock any Send stuck on a write to a non-reading child),
// then CloseRequest, then CloseOwnership, then closeResponse; joins any
// real cleanup errors, caches the result, and blocks all concurrent
// callers until finished. On a failed Kill the child is not reaped;
// Close still closes every parent pipe end, without the I/O mutexes, so
// blocked Send and Receive calls return. The cached result reports the
// cleanup as incomplete and preserves the real kill error; closeOnce is
// never reset, so Close never retries by itself. An explicit later Kill
// on a valid handle may still succeed and reap the child; the cached
// Close error is a historical record and does not change. Close and
// Detach share one terminal lifecycle, so whichever is called first
// wins: a Close after a completed Detach returns the cached detach
// result and never kills the released child. Close is nil-safe and
// idempotent.
func (p *roleProcess) Close() error {
	if p == nil {
		return nil
	}
	p.closeOnce.Do(func() {
		defer close(p.closeDone)
		var errs []error

		// Kill and reap the child process before acquiring sendMu for
		// CloseRequest. A pending Send may be blocked in a write call
		// waiting for a child that is not reading; killing the child
		// unblocks that write so Close can acquire sendMu instead of
		// deadlocking between Kill (which waits for Send to release
		// sendMu) and CloseRequest (which needs sendMu).
		if cerr := p.Kill(); cerr != nil {
			// The kill failed, so the child may still be running and a
			// blocked Send or Receive still holds sendMu or receiveMu.
			// Close the parent pipe ends under their own close mutexes
			// only: taking the I/O mutexes here would wait for exactly
			// the goroutines these closes are meant to release. The
			// child is not reaped, so this cleanup is incomplete and the
			// cached result says so.
			errs = append(errs, cerr)
			if rerr := p.closeRequestWriter(); rerr != nil {
				errs = append(errs, rerr)
			}
			if rerr := p.CloseOwnership(); rerr != nil {
				errs = append(errs, rerr)
			}
			if rerr := p.closeResponseReader(); rerr != nil {
				errs = append(errs, rerr)
			}
			p.closeErr = fmt.Errorf("role process close: cleanup incomplete: %w", errors.Join(errs...))
			return
		}

		// Close the request writer normally; the child is already gone
		// so any blocked write has returned and sendMu is available.
		if cerr := p.CloseRequest(); cerr != nil {
			errs = append(errs, cerr)
		}

		// Close the ownership writer (supervisors get nil, which is a no-op).
		if cerr := p.CloseOwnership(); cerr != nil {
			errs = append(errs, cerr)
		}

		// Close the response reader.
		if cerr := p.closeResponse(); cerr != nil {
			errs = append(errs, cerr)
		}

		if len(errs) > 0 {
			p.closeErr = fmt.Errorf("role process close: %w", errors.Join(errs...))
		}
	})
	<-p.closeDone
	return p.closeErr
}

// Detach relinquishes an accepted supervisor child without killing or
// waiting for it. It is valid only for roleSupervisor; detaching a
// worker-host is rejected before any lifecycle state changes. All
// Send/Receive frame I/O must be quiescent before Detach is called:
// the caller must not race Detach with Send, Receive, or Close. It
// must first decide whether the supervisor handshake result was
// accepted or rejected and then choose exactly one terminal action.
// Detach occupies the same terminal closeOnce/closeDone/closeErr
// lifecycle as Close, so a later deferred Close observes the completed
// lifecycle, returns the cached detach result, and never kills the
// child. Within the detach lifecycle it closes the parent request
// writer and the parent response reader, and it calls
// cmd.Process.Release. Release only drops the Go handle so the starter
// can never reap the child through Wait; it does not itself reparent
// the child. The starter's imminent process exit is what lets the OS
// reparent the still-running supervisor. Every step is attempted and
// their errors are joined. It never calls Kill, Wait, CloseOwnership,
// or signals the child. Detach is nil-safe and idempotent: repeated
// calls return the cached result.
func (p *roleProcess) Detach() error {
	if p == nil {
		return nil
	}
	if p.role != roleSupervisor {
		return fmt.Errorf("role process detach: only %q may detach, process role is %q", roleSupervisor, p.role)
	}
	p.closeOnce.Do(func() {
		defer close(p.closeDone)
		var errs []error

		// Close the request writer and the response reader so the child
		// observes EOF on both pipes once it stops using them.
		if cerr := p.CloseRequest(); cerr != nil {
			errs = append(errs, cerr)
		}
		if cerr := p.closeResponse(); cerr != nil {
			errs = append(errs, cerr)
		}

		// Release the os.Process handle without killing or waiting;
		// the accepted supervisor continues independently.
		if p.cmd != nil && p.cmd.Process != nil {
			if rerr := p.cmd.Process.Release(); rerr != nil {
				errs = append(errs, fmt.Errorf("release role process: %w", rerr))
			}
		}

		if len(errs) > 0 {
			p.closeErr = fmt.Errorf("role process detach: %w", errors.Join(errs...))
		}
	})
	<-p.closeDone
	return p.closeErr
}

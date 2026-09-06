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
type roleProcess struct {
	cmd            *exec.Cmd
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

	// respCloseMu guards the idempotent close of the response reader.
	respCloseMu  sync.Mutex
	respClosed   bool
	respCloseErr error

	// closeOnce guarantees Close performs its full cleanup exactly once;
	// all concurrent callers block until Close finishes and then receive
	// the same cached result.
	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
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

	cmd := exec.Command(executable, string(r))
	cmd.ExtraFiles = []*os.File{requestReader, responseWriter}

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
		if len(closeErrs) == 1 {
			return nil, closeErrs[0]
		}
		return nil, errors.Join(closeErrs...)
	}

	// The child now owns its copies of the request reader and response
	// writer; close the parent's copies so the pipe ends observe EOF as
	// soon as the child exits.
	closeErrs := make([]error, 0, 4)
	if err := requestReader.Close(); err != nil {
		closeErrs = append(closeErrs, fmt.Errorf("close child request reader: %w", err))
	}
	if err := responseWriter.Close(); err != nil {
		closeErrs = append(closeErrs, fmt.Errorf("close child response writer: %w", err))
	}
	if len(closeErrs) > 0 {
		// A surviving child would keep the request pipe open forever;
		// close the parent ends, kill the exact child, and reap it.
		if err := requestWriter.Close(); err != nil {
			closeErrs = append(closeErrs, fmt.Errorf("close request writer: %w", err))
		}
		if err := responseReader.Close(); err != nil {
			closeErrs = append(closeErrs, fmt.Errorf("close response reader: %w", err))
		}
		if err := cmd.Process.Kill(); err != nil {
			closeErrs = append(closeErrs, fmt.Errorf("kill role process: %w", err))
		}
		if err := cmd.Wait(); err != nil {
			closeErrs = append(closeErrs, fmt.Errorf("wait role process: %w", err))
		}
		return nil, errors.Join(closeErrs...)
	}

	return &roleProcess{
		cmd:            cmd,
		requestWriter:  requestWriter,
		responseReader: responseReader,
		frameLimit:     privateFrameLimit,
		done:           make(chan struct{}),
		closeDone:      make(chan struct{}),
	}, nil
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

// Kill terminates the child process. It is nil-safe and idempotent:
// if the process is already gone it returns nil; otherwise it kills
// cmd.Process, always calls Wait to reap the zombie, and returns any
// unexpected Kill infrastructure errors. A normal non-zero exit from
// Wait is treated as a child outcome, not a cleanup failure.
func (p *roleProcess) Kill() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	err := p.cmd.Process.Kill()
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("kill role process: %w", err)
	}
	// Reap the process regardless of whether Kill succeeded.
	_ = p.Wait()
	return nil
}

// Close performs a full lifecycle teardown: Kill/reap the exact child
// first (to unblock any Send stuck on a write to a non-reading child),
// then CloseRequest, then closeResponse; joins any real cleanup errors,
// caches the result, and blocks all concurrent callers until finished.
// It is nil-safe and idempotent.
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
			errs = append(errs, cerr)
		}

		// Close the request writer normally; the child is already gone
		// so any blocked write has returned and sendMu is available.
		if cerr := p.CloseRequest(); cerr != nil {
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

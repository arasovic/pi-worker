//go:build darwin || linux

package background

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
)

const (
	childRoleRequestFD  = 3
	childRoleResponseFD = 4
)

// childRolePipes owns the pair of file handles wrapping the fixed
// descriptors used by a spawned child to exchange role information.
type childRolePipes struct {
	requestReader  *os.File // fd 3
	responseWriter *os.File // fd 4
	closed         bool
	mu             sync.Mutex
	closeErr       error // set after first Close attempt
}

// openChildRolePipes wraps fd 3 as a read-only request reader and
// fd 4 as a write-only response writer.  Each descriptor is verified
// via Stat.  If a secondary Stat fails the first handle is closed
// before returning; if NewFile fails for the second handle the
// first handle is also closed.  Close failures are joined with the
// primary error so that callers can inspect individual causes.
func openChildRolePipes() (*childRolePipes, error) {
	req := os.NewFile(childRoleRequestFD, "/dev/fd/3")
	if req == nil {
		return nil, fmt.Errorf("open child request fd %d: NewFile returned nil", childRoleRequestFD)
	}
	if _, err := req.Stat(); err != nil {
		closeErrs := []error{fmt.Errorf("stat child request fd %d: %w", childRoleRequestFD, err)}
		if cErr := req.Close(); cErr != nil {
			closeErrs = append(closeErrs, fmt.Errorf("close request fd: %w", cErr))
		}
		if len(closeErrs) == 1 {
			return nil, closeErrs[0]
		}
		return nil, errors.Join(closeErrs...)
	}
	// Ensure future execs (and their children) never inherit role transport ends,
	// which would mask EOF and break process teardown.
	syscall.CloseOnExec(int(req.Fd()))

	resp := os.NewFile(childRoleResponseFD, "/dev/fd/4")
	if resp == nil {
		errMsg := fmt.Errorf("open child response fd %d: NewFile returned nil", childRoleResponseFD)
		if cErr := req.Close(); cErr != nil {
			return nil, errors.Join(errMsg, fmt.Errorf("close request fd: %w", cErr))
		}
		return nil, errMsg
	}
	if _, err := resp.Stat(); err != nil {
		closeErrs := []error{fmt.Errorf("stat child response fd %d: %w", childRoleResponseFD, err)}
		if cErr := resp.Close(); cErr != nil {
			closeErrs = append(closeErrs, fmt.Errorf("close response fd: %w", cErr))
		}
		if cErr := req.Close(); cErr != nil {
			closeErrs = append(closeErrs, fmt.Errorf("close request fd: %w", cErr))
		}
		if len(closeErrs) == 1 {
			return nil, closeErrs[0]
		}
		return nil, errors.Join(closeErrs...)
	}
	syscall.CloseOnExec(int(resp.Fd()))

	return &childRolePipes{
		requestReader:  req,
		responseWriter: resp,
	}, nil
}

// Close releases both descriptors, exactly once.
// It is nil-safe and idempotent.
func (p *childRolePipes) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return p.closeErr
	}
	p.closed = true

	var closeErrs []error

	if p.requestReader != nil {
		if err := p.requestReader.Close(); err != nil {
			closeErrs = append(closeErrs, fmt.Errorf("close request fd: %w", err))
		}
		p.requestReader = nil
	}

	if p.responseWriter != nil {
		if err := p.responseWriter.Close(); err != nil {
			closeErrs = append(closeErrs, fmt.Errorf("close response fd: %w", err))
		}
		p.responseWriter = nil
	}

	if len(closeErrs) > 0 {
		p.closeErr = errors.Join(closeErrs...)
		return p.closeErr
	}
	return nil
}

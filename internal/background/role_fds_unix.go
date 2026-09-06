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
	childRoleRequestFD   = 3
	childRoleResponseFD  = 4
	childRoleOwnershipFD = 5
)

// childRolePipes owns the file handles wrapping the fixed descriptors
// used by a spawned child to exchange role information.
// The ownershipReader end is only present when r is roleWorkerHost.
type childRolePipes struct {
	requestReader   *os.File // fd 3
	responseWriter  *os.File // fd 4
	ownershipReader *os.File // fd 5 (only for worker-host)
	closed          bool
	mu              sync.Mutex
	closeErr        error // set after first Close attempt
}

// openChildRolePipes wraps fd 3 as a read-only request reader and
// fd 4 as a write-only response writer.  Each descriptor is verified
// via Stat.  For worker-host roles an additional ownership fd 5 is
// opened, Stat'd, and included in the returned pipes.
//
// Close failures are joined with the primary error so that callers
// can inspect individual causes.
func openChildRolePipes(r role) (*childRolePipes, error) {
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

	var owner *os.File
	if r == roleWorkerHost {
		owner = os.NewFile(childRoleOwnershipFD, "/dev/fd/5")
		if owner == nil {
			errMsg := fmt.Errorf("open child ownership fd %d: NewFile returned nil", childRoleOwnershipFD)
			var closeErrs []error
			if cErr := resp.Close(); cErr != nil {
				closeErrs = append(closeErrs, fmt.Errorf("close response fd: %w", cErr))
			}
			if cErr := req.Close(); cErr != nil {
				closeErrs = append(closeErrs, fmt.Errorf("close request fd: %w", cErr))
			}
			if len(closeErrs) == 0 {
				return nil, errMsg
			}
			return nil, errors.Join(errMsg, errors.Join(closeErrs...))
		}
		if _, err := owner.Stat(); err != nil {
			var closeErrs []error
			closeErrs = append(closeErrs, fmt.Errorf("stat child ownership fd %d: %w", childRoleOwnershipFD, err))
			if cErr := owner.Close(); cErr != nil {
				closeErrs = append(closeErrs, fmt.Errorf("close ownership fd: %w", cErr))
			}
			if cErr := resp.Close(); cErr != nil {
				closeErrs = append(closeErrs, fmt.Errorf("close response fd: %w", cErr))
			}
			if cErr := req.Close(); cErr != nil {
				closeErrs = append(closeErrs, fmt.Errorf("close request fd: %w", cErr))
			}
			return nil, errors.Join(closeErrs...)
		}
		syscall.CloseOnExec(int(owner.Fd()))
	}

	return &childRolePipes{
		requestReader:   req,
		responseWriter:  resp,
		ownershipReader: owner,
	}, nil
}

// Close releases the request, response, and ownership (when present)
// descriptors, exactly once. It is nil-safe and idempotent.
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

	if p.ownershipReader != nil {
		if err := p.ownershipReader.Close(); err != nil {
			closeErrs = append(closeErrs, fmt.Errorf("close ownership fd: %w", err))
		}
		p.ownershipReader = nil
	}

	if len(closeErrs) > 0 {
		p.closeErr = errors.Join(closeErrs...)
		return p.closeErr
	}
	return nil
}

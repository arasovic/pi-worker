//go:build !darwin && !linux

package background

// roleProcess mirrors the Unix type name on unsupported platforms. It is
// never constructed successfully because startRoleProcess always fails
// before any pipe or process creation.
type roleProcess struct {
	frameLimit int // limit for Send/Receive frames; unused on this platform
}

// startRoleProcess returns errRoleProcessUnsupported before creating any
// pipes or a child process on unsupported platforms.
func startRoleProcess(string, role) (*roleProcess, error) {
	return nil, errRoleProcessUnsupported
}

// Send rejects every call on unsupported platforms.
func (p *roleProcess) Send([]byte) error {
	return errRoleProcessUnsupported
}

// Receive rejects every call on unsupported platforms.
func (p *roleProcess) Receive() ([]byte, error) {
	return nil, errRoleProcessUnsupported
}

// CloseRequest rejects every call on unsupported platforms.
func (p *roleProcess) CloseRequest() error {
	return errRoleProcessUnsupported
}

// Wait returns errRoleProcessUnsupported on unsupported platforms.
func (p *roleProcess) Wait() error {
	return errRoleProcessUnsupported
}

// Kill returns errRoleProcessUnsupported on unsupported platforms.
func (p *roleProcess) Kill() error {
	return errRoleProcessUnsupported
}

// Close is nil-safe and idempotent on unsupported platforms:
// it returns nil for a nil receiver and errRoleProcessUnsupported otherwise.
func (p *roleProcess) Close() error {
	if p == nil {
		return nil
	}
	return errRoleProcessUnsupported
}

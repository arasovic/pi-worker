//go:build !darwin && !linux

package background

const (
	childRoleRequestFD  = 3
	childRoleResponseFD = 4
)

type childRolePipes struct{}

// openChildRolePipes returns errRoleProcessUnsupported without touching
// any file descriptors on unsupported platforms.
func openChildRolePipes() (*childRolePipes, error) {
	return nil, errRoleProcessUnsupported
}

// Close is a no-op on unsupported platforms.
func (p *childRolePipes) Close() error {
	if p == nil {
		return nil
	}
	return nil
}

//go:build !darwin && !linux

package admission

// lockState is not supported on this platform. It returns
// ErrUnsupported so callers can classify the failure.
func lockState(string) (func(), error) {
	return nil, ErrUnsupported
}

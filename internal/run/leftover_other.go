//go:build !darwin && !linux

package run

// defaultProcessEnviron reports no environment on platforms with no
// reader, so no process is ever reported as a leftover.
func defaultProcessEnviron(_ int32) ([]string, error) {
	return nil, nil
}

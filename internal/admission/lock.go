// Package admission provides durable state for the admission controller:
// a schema-versioned document of ordered tickets that gates and schedules
// prompt execution on worker processes.
package admission

import "errors"

// ErrUnsupported is returned by lockState on platforms that do not
// support POSIX advisory file locking. Callers can use errors.Is to
// classify the failure.
var ErrUnsupported = errors.New("admission lock: not supported on this platform")

package background

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/arasovic/pi-worker/internal/pi"
)

// TestWorkerHostAdapterUnsupportedPlatformOrder locks the platform guard
// order of workerHostAdapter.Run. On platforms that cannot start role
// processes, every request — valid and invalid alike — must answer with
// the existing errRoleProcessUnsupported result before any request
// validation, so an invalid request can never masquerade as a
// validation failure where no host could ever run. On supported
// platforms the request validation still runs first and an invalid
// request is a validation failure, never an unsupported-platform
// result. The test is deliberately free of build tags and unix-only
// helpers so it compiles and runs on every platform.
func TestWorkerHostAdapterUnsupportedPlatformOrder(t *testing.T) {
	a := newWorkerHostAdapter("", "") // the executables are never consulted
	valid := pi.WorkerRequest{
		Model:     "acme/m-1",
		Prompt:    "run the focused task",
		Workspace: "ws",
		WorkerID:  1,
	}
	invalid := pi.WorkerRequest{}

	switch runtime.GOOS {
	case "darwin", "linux":
		// Supported platforms validate first: the invalid request fails
		// validation without spawning anything and without any
		// unsupported-platform claim.
		result := a.Run(context.Background(), invalid)
		if result.Status != pi.StatusFailed {
			t.Fatalf("invalid request result = %+v, want the validation failure", result)
		}
		if !strings.Contains(result.Error, "model is required") {
			t.Fatalf("invalid request error %q does not report the missing model", result.Error)
		}
	default:
		// Unsupported platforms answer before validation: a valid
		// request without even an execution deadline and an invalid
		// request both receive the same unsupported result.
		for name, req := range map[string]pi.WorkerRequest{"valid": valid, "invalid": invalid} {
			t.Run(name, func(t *testing.T) {
				result := a.Run(context.Background(), req)
				if result.Status != pi.StatusUnavailable {
					t.Fatalf("result = %+v, want unavailable on unsupported platform %s", result, runtime.GOOS)
				}
				if result.Model != req.Model {
					t.Fatalf("result model = %q, want %q", result.Model, req.Model)
				}
				want := "start worker host: " + errRoleProcessUnsupported.Error()
				if result.Error != want {
					t.Fatalf("result error = %q, want the existing unsupported role-process result %q", result.Error, want)
				}
			})
		}
	}
}

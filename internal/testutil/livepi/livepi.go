// Package livepi decides whether the host can run tests that exercise a real
// Pi binary against a real authenticated model. Such tests must report why
// they did not run rather than pass vacuously, so every not-ready outcome
// carries a reason naming exactly which requirement failed.
package livepi

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/piversion"
)

// ExecutableName is the binary the lookup must resolve.
const ExecutableName = "pi"

// ModelEnvVar holds the model selector for live tests. It has no default:
// an unset or empty value is never substituted with a fallback.
const ModelEnvVar = "PI_WORKER_LIVE_MODEL"

// Result is the outcome of the readiness decision. Exactly one of Ready or
// NotReady.Reason is meaningful.
type Result struct {
	Ready      bool
	Executable string
	Model      string
	Reason     string
}

// Decide reports whether the host is ready for live-Pi tests. lookup and probe
// are injected so callers (and tests) can supply the real resolver and version
// probe or fakes.
//
// The three requirements are checked in order and each failure yields its own
// reason. A malformed model selector is deliberately not rejected here: it
// must reach the command line and fail loudly there instead of becoming a
// silent skip.
func Decide(model string, lookup func(string) (string, error), probe func(ctx context.Context, executable string) (string, error)) Result {
	if model == "" {
		return Result{Reason: "no model: " + ModelEnvVar + " is unset or empty"}
	}
	executable, err := lookup(ExecutableName)
	if err != nil {
		return Result{Reason: "no " + ExecutableName + " executable on PATH: " + err.Error()}
	}
	output, err := probe(context.TODO(), executable)
	if err != nil {
		return Result{Reason: ExecutableName + " version probe failed: " + err.Error()}
	}
	classification := piversion.Classify(output)
	switch classification.Status {
	case piversion.StatusVerified:
		return Result{Ready: true, Executable: executable, Model: model}
	case piversion.StatusUnverified:
		return Result{Reason: ExecutableName + " version " + classification.Version + " is not the verified version " + piversion.VerifiedVersion}
	default:
		return Result{Reason: ExecutableName + " version output is not a semantic version"}
	}
}

// ProbeTimeout bounds the version probe in Gate.
const ProbeTimeout = 5 * time.Second

// Gate gathers the real inputs from the host, applies Decide, and, when the
// host is not ready, fails the test with the reason if PI_WORKER_LIVE_REQUIRED=1
// or skips the test with the reason otherwise. The required mode exists for the
// deliberate probe command, so a probe that cannot run reports failure instead
// of exiting zero.
func Gate(t *testing.T) Result {
	t.Helper()
	result := Decide(
		os.Getenv(ModelEnvVar),
		exec.LookPath,
		func(ctx context.Context, executable string) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
			defer cancel()
			return piversion.Probe(ctx, executable)
		},
	)
	if !result.Ready {
		if os.Getenv("PI_WORKER_LIVE_REQUIRED") == "1" {
			t.Fatal(result.Reason)
		}
		t.Skip(result.Reason)
	}
	return result
}

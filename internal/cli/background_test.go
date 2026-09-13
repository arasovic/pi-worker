package cli

import (
	"errors"
	"fmt"
	"testing"

	"github.com/arasovic/pi-worker/internal/background"
	"github.com/arasovic/pi-worker/internal/contracts"
)

// TestBackgroundStartExitCodeOversizedRequestIsUsage verifies that a start
// the handoff refused because the encoded request exceeded the background
// frame limit exits as a usage error — the caller can shrink the input or
// run in the foreground — while a start failure with no such classification
// stays internal.
func TestBackgroundStartExitCodeOversizedRequestIsUsage(t *testing.T) {
	oversized := fmt.Errorf("background manager start: %w", background.ErrStartRequestTooLarge)
	if got, want := backgroundStartExitCode(oversized), 2; got != want {
		t.Fatalf("oversized start exit code = %d, want %d", got, want)
	}
	if got, want := backgroundStartExitCode(oversized), contracts.ExitCode(contracts.RunFailed, &contracts.RunError{Kind: contracts.ErrorUsage}); got != want {
		t.Fatalf("oversized start exit code = %d, want the usage exit code %d", got, want)
	}

	plain := errors.New("background manager start: injected internal failure")
	if got, want := backgroundStartExitCode(plain), 9; got != want {
		t.Fatalf("plain start failure exit code = %d, want %d", got, want)
	}
	if got, want := backgroundStartExitCode(plain), contracts.ExitCode(contracts.RunFailed, &contracts.RunError{Kind: contracts.ErrorInternal}); got != want {
		t.Fatalf("plain start failure exit code = %d, want the internal exit code %d", got, want)
	}
}

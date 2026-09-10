//go:build darwin || linux

package background

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestDispatchSupervisorRoleWithoutRolePipes hands the supervisor role
// token to a fresh process of the production binary, so that process calls
// DispatchRole with the token and no role transport descriptors inherited:
// dispatchSupervisorRole must report the token as handled with
// roleExitPipesUnavailable and must not print the old refusal text. The
// dispatch runs in the child rather than in the test binary because the test
// binary keeps its own descriptors at the fixed role fds, so only a freshly
// exec'd program can honestly have no role pipes to open.
func TestDispatchSupervisorRoleWithoutRolePipes(t *testing.T) {
	var stderr strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, piWorkerBin(t), string(roleSupervisor))
	cmd.Stderr = &stderr
	err := cmd.Run()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("child error = %v, want a failure exit status; stderr:\n%s", err, stderr.String())
	}
	if got := exitErr.ExitCode(); got != roleExitPipesUnavailable {
		t.Fatalf("child exit code = %d, want %d (roleExitPipesUnavailable); stderr:\n%s",
			got, roleExitPipesUnavailable, stderr.String())
	}
	if text := stderr.String(); strings.Contains(text, "is not dispatched yet") {
		t.Fatalf("child still refuses the supervisor token; stderr:\n%s", text)
	}
	if text := stderr.String(); !strings.Contains(text, string(roleSupervisor)) {
		t.Fatalf("child reported no %s diagnostic; stderr:\n%s", roleSupervisor, text)
	}
}

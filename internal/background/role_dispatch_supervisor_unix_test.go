//go:build darwin || linux

package background

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// supervisorDispatchSignalChildEnv is the environment variable that
// switches a spawned roleSupervisor test child out of its echo loop into
// the opt-in dispatch-signal mode: the child replaces
// receiveSupervisorStartFunc with a seam that SIGTERMs its own process,
// waits for the signal to be delivered, and then rejects the start with
// no preparation before running dispatchSupervisorRole. Unset or empty
// keeps the echo behavior every earlier role-process test relies on;
// TestMain reads this variable.
const supervisorDispatchSignalChildEnv = "PI_WORKER_BACKGROUND_TEST_SUPERVISOR_DISPATCH_SIGNAL"

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

// TestDispatchSupervisorRoleSurvivesSignalDuringExchange proves that a
// SIGTERM delivered while the supervisor start exchange is in flight does
// not kill the supervisor child. The child runs the production
// dispatchSupervisorRole over a real role transport but with
// receiveSupervisorStartFunc replaced by a seam that signals its own
// process and then rejects the start with no preparation: with the
// interception installed before the exchange, the signal only cancels a
// context the rejected path never consults, so the child reaches its
// ordinary rejection exit. Without the interception the default SIGTERM
// disposition kills the child, and the parent observes a signal death
// instead of the role exit code.
func TestDispatchSupervisorRoleSurvivesSignalDuringExchange(t *testing.T) {
	t.Setenv(supervisorDispatchSignalChildEnv, "1")
	p, err := startRoleProcess(testExe(t), roleSupervisor)
	if err != nil {
		t.Fatalf("startRoleProcess: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	waitErr := p.Wait()
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		t.Fatalf("child Wait = %v, want *exec.ExitError(%d)", waitErr, roleExitExchangeFailed)
	}
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		t.Fatalf("child died by signal %v during the exchange, want a voluntary exit; the interception must be installed before receiveSupervisorStart", status.Signal())
	}
	if got := exitErr.ExitCode(); got != roleExitExchangeFailed {
		t.Fatalf("child exit code = %d, want %d (roleExitExchangeFailed)", got, roleExitExchangeFailed)
	}
}

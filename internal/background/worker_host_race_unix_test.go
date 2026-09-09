//go:build darwin || linux

package background

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/pi"
)

// TestWorkerHostRaceAdapterCancellationVersusTerminalResult races
// ownership-close cancellation against the host's terminal result: the
// run may complete before the cancellation lands or be cancelled by it,
// and whichever terminal frame the drain receives must be returned
// unchanged — never a fabricated result, never a leaked goroutine or
// descriptor.
func TestWorkerHostRaceAdapterCancellationVersusTerminalResult(t *testing.T) {
	for i := 0; i < 5; i++ {
		fdsBefore := countFDs(t)
		gosBefore := runtime.NumGoroutine()
		a := startHostExecutingAdapter(t)
		pidPath := filepath.Join(t.TempDir(), "fakepi.pid")
		setupFakePiEnv(t, stuckPathScript())
		t.Setenv("FAKEPI_PIDFILE", pidPath)

		ctx, cancel := context.WithTimeout(context.Background(), workerHostRunTimeout)
		done := make(chan pi.WorkerResult, 1)
		go func() { done <- a.Run(ctx, adapterWorkerRequest(t.TempDir())) }()
		piPID := readPIDFile(t, pidPath)
		// Cancel at a random moment while the run is in flight.
		time.Sleep(time.Duration(i%3) * 20 * time.Millisecond)
		cancel()
		select {
		case result := <-done:
			switch result.Status {
			case pi.StatusCompleted, pi.StatusCancelled:
			default:
				t.Fatalf("iteration %d: result = %+v, want completed or cancelled", i, result)
			}
			if strings.Contains(result.Error, "force-killed") {
				t.Fatalf("iteration %d: result %q claims a force-kill", i, result.Error)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("iteration %d: Run did not return within the bounded wait", i)
		}
		waitProcessGone(t, piPID)
		requireNoWorkerHostLeak(t, fdsBefore, gosBefore)
	}
}

// TestWorkerHostRaceOwnerLossDuringExchange drives repeated in-process
// handler exchanges with ownership closed at a random moment mid-run:
// the handler must always return within the bound with a defined
// terminal outcome, Pi must always be cleaned up, and no goroutine may
// outlive the exchange.
func TestWorkerHostRaceOwnerLossDuringExchange(t *testing.T) {
	for i := 0; i < 5; i++ {
		// Warm gopsutil's one-time probe before the baseline: that setup
		// is deliberately outside the measured exchange below.
		warmUpGopsutilSelfProbe(t)
		fdsBefore := countFDs(t)
		gosBefore := runtime.NumGoroutine()
		pidPath := filepath.Join(t.TempDir(), "fakepi.pid")
		setupFakePiEnv(t, stuckPathScript())
		t.Setenv("FAKEPI_PIDFILE", pidPath)

		fx := newWorkerHostFixture(t)
		req := validWorkerHostRequest()
		req.workspace = t.TempDir()
		req.piExecutable = fakePiBin(t)
		sendWorkerHostRequest(t, fx, req)
		done := startWorkerHostChild(fx)
		piPID := readPIDFile(t, pidPath)
		time.Sleep(time.Duration(i%3) * 15 * time.Millisecond)
		if err := fx.ownershipWriter.Close(); err != nil {
			t.Fatalf("iteration %d: close ownership writer: %v", i, err)
		}
		fx.ownershipWriter = nil

		out := waitWorkerHostChild(t, done, 30*time.Second)
		if out.err != nil {
			t.Fatalf("iteration %d: receiveWorkerHost: %v", i, out.err)
		}
		if out.exchange.result.Status != pi.StatusCancelled {
			t.Fatalf("iteration %d: exchange result = %+v, want cancelled", i, out.exchange.result)
		}
		waitProcessGone(t, piPID)
		// This iteration's fixture parent ends are closed only now — after
		// the handler finished its cancellation cleanup — so the leak
		// check sees no descriptor the fixture itself still owns.
		fx.close()
		requireNoWorkerHostLeak(t, fdsBefore, gosBefore)
	}
}

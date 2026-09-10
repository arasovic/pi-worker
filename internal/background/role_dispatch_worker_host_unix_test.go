//go:build darwin || linux

package background

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/arasovic/pi-worker/internal/pi"
)

// pi-worker binary build state: the production binary is built once per
// test run into a directory TestMain removes after the run, exactly like
// the fakepi helper binary beside it.
var (
	piWorkerBuildOnce sync.Once
	piWorkerBinPath   string
	piWorkerBinDir    string
	piWorkerBuildErr  error
)

// buildPiWorkerOnce builds ./cmd/pi-worker once per test run.
func buildPiWorkerOnce() {
	piWorkerBuildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "pi-worker-bg-role-bin-*")
		if err != nil {
			piWorkerBuildErr = fmt.Errorf("create pi-worker build directory: %w", err)
			return
		}
		piWorkerBinDir = dir
		piWorkerBinPath = filepath.Join(dir, "pi-worker")
		build := exec.Command("go", "build", "-o", piWorkerBinPath, "github.com/arasovic/pi-worker/cmd/pi-worker")
		if out, err := build.CombinedOutput(); err != nil {
			piWorkerBuildErr = fmt.Errorf("build pi-worker: %v\n%s", err, out)
			_ = os.RemoveAll(dir)
			piWorkerBinDir = ""
			return
		}
	})
}

// removePiWorkerBuildDir removes the per-run production binary build
// directory; it is called by TestMain after the test run.
func removePiWorkerBuildDir() {
	if piWorkerBinDir != "" {
		_ = os.RemoveAll(piWorkerBinDir)
	}
}

// piWorkerBin returns the built production pi-worker binary path,
// building it on first use.
func piWorkerBin(t *testing.T) string {
	t.Helper()
	buildPiWorkerOnce()
	if piWorkerBuildErr != nil {
		t.Fatalf("build pi-worker: %v", piWorkerBuildErr)
	}
	return piWorkerBinPath
}

// TestWorkerHostAdapterRunsProductionRoleBinary runs one task through
// the adapter with the built production binary as its role executable and
// the built fake Pi helper as the Pi executable, and requires the
// terminal result. No test-only child mode is in play: the spawned child
// reaches the real worker-host exchange only through the dispatch the
// users' own binary carries, so this is the one place where the child
// half is proven against the binary that ships rather than against the
// test binary.
func TestWorkerHostAdapterRunsProductionRoleBinary(t *testing.T) {
	fdsBefore := countFDs(t)
	gosBefore := runtime.NumGoroutine()
	a := newWorkerHostAdapter(piWorkerBin(t), fakePiBin(t))
	setupFakePiEnv(t, happyPathScript("production role answer"))

	ctx, cancel := context.WithTimeout(context.Background(), workerHostRunTimeout)
	defer cancel()
	result := a.Run(ctx, adapterWorkerRequest(t.TempDir()))

	if result.Status != pi.StatusCompleted {
		t.Fatalf("result = %+v, want a terminal completed result from the production role child", result)
	}
	if result.Explanation != "production role answer" {
		t.Fatalf("explanation = %q, want the fake Pi answer", result.Explanation)
	}
	if result.Model != "acme/m-1" {
		t.Fatalf("result model = %q", result.Model)
	}
	requireNoWorkerHostLeak(t, fdsBefore, gosBefore)
}

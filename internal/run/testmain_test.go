package run

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// TestMain replaces the runProcesses seam so no test scans the host's
// real process table through the controller, and it verifies that every
// test which runs a controller with a run id restores the run marker via
// t.Setenv instead of leaking PI_WORKER_RUN into later tests.
func TestMain(m *testing.M) {
	original, wasSet := os.LookupEnv(RunMarkerEnv)
	runProcesses = func(string, time.Time) []LeftoverProcess { return nil }

	code := m.Run()

	current, isSet := os.LookupEnv(RunMarkerEnv)
	if code == 0 && (wasSet != isSet || original != current) {
		fmt.Fprintln(os.Stderr, `a test left PI_WORKER_RUN changed; call t.Setenv(RunMarkerEnv, "") in any test that runs a controller with a run id`)
		os.Exit(1)
	}
	os.Exit(code)
}

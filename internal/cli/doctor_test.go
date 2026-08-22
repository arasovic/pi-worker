package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/config"
	"github.com/arasovic/pi-worker/internal/doctor"
	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/run"
	"github.com/arasovic/pi-worker/internal/testutil/fakepi/script"
)

func readyDoctorDependencies() doctor.Dependencies {
	return doctor.Dependencies{
		Lookup:  func(string) (string, error) { return "pi", nil },
		Version: func(context.Context, string) (string, error) { return "0.84.2", nil },
		LoadConfig: func() (config.Config, error) {
			return config.Config{SchemaVersion: 1, DefaultModel: "acme/model"}, nil
		},
		Catalog:   &fakeCatalog{models: []pi.ModelProjection{{Provider: "acme", ID: "model"}}},
		Workspace: func() (string, error) { return ".", nil },
		Inspect:   func(context.Context, string) (*run.GitState, error) { return &run.GitState{}, nil },
	}
}

func installDoctorDependencies(t *testing.T, deps doctor.Dependencies) {
	t.Helper()
	original := newDoctorDependencies
	newDoctorDependencies = func(debug *pi.DebugSink) doctor.Dependencies {
		deps.Debug = debug
		return deps
	}
	t.Cleanup(func() { newDoctorDependencies = original })
}

func TestDoctorHumanOutputKeepsSixChecksInOrder(t *testing.T) {
	// This catches a CLI formatter that hides a check or changes the runner's
	// contract order, making readiness diagnosis ambiguous.
	installDoctorDependencies(t, readyDoctorDependencies())
	code, stdout, stderr := runCLI(t, []string{"doctor"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	want := []string{
		"pi-executable: ok - Pi executable found", "pi-version: ok - Pi version 0.84.2 is supported", "config: ok - Pi-worker configuration is valid", "model-catalog: ok - Pi model catalog is available", "default-model: ok - Configured default model is available", "workspace: ok - Workspace is a git work tree; a run can report what it changed and check declared writes.", "ready: yes",
	}
	for i, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if i >= len(want) || line != want[i] {
			t.Fatalf("output line %d = %q, want %q\nfull output: %q", i, line, want[i], stdout)
		}
	}
	if len(strings.Split(strings.TrimSpace(stdout), "\n")) != len(want) {
		t.Fatalf("output = %q, want %d concise lines", stdout, len(want))
	}
}

func TestDoctorJSONIsOneDocumentAndDebugStaysOnStderr(t *testing.T) {
	// This catches debug diagnostics corrupting the machine-readable result.
	deps := readyDoctorDependencies()
	deps.Catalog = &fakeCatalog{list: func(_ context.Context, req pi.CatalogRequest) ([]pi.ModelProjection, error) {
		req.Debug.Worker(1).Log("rpc=get_available_models", "status=completed")
		return []pi.ModelProjection{{Provider: "acme", ID: "model"}}, nil
	}}
	installDoctorDependencies(t, deps)
	code, stdout, stderr := runCLI(t, []string{"doctor", "--json", "--debug"}, "")
	if code != 0 || strings.Count(strings.TrimSpace(stdout), "\n") != 0 || strings.Contains(stdout, "[pi-worker +") || !strings.Contains(stderr, "[pi-worker +") {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	var result doctor.Result
	if err := json.Unmarshal([]byte(stdout), &result); err != nil || len(result.Checks) != 6 || result.SchemaVersion != 1 {
		t.Fatalf("JSON = %q, result = %#v, err = %v", stdout, result, err)
	}
}

func TestDoctorExitClassification(t *testing.T) {
	// This catches each public exit class being collapsed into a generic error.
	for _, test := range []struct {
		name       string
		ctx        context.Context
		deps       func() doctor.Dependencies
		want       int
		wantOutput bool
	}{
		{"ready", context.Background(), readyDoctorDependencies, 0, true},
		{"readiness", context.Background(), func() doctor.Dependencies {
			d := readyDoctorDependencies()
			d.Lookup = func(string) (string, error) { return "", errors.New("missing") }
			return d
		}, 3, true},
		{"internal", context.Background(), func() doctor.Dependencies {
			d := readyDoctorDependencies()
			d.Catalog = &fakeCatalog{err: &pi.ProtocolError{Message: "bad"}}
			return d
		}, 9, false},
		{"timeout", expiredDoctorContext(t, context.DeadlineExceeded), readyDoctorDependencies, 7, false},
		{"cancellation", expiredDoctorContext(t, context.Canceled), readyDoctorDependencies, 8, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			installDoctorDependencies(t, test.deps())
			code, stdout, _ := runCLIWithContext(t, test.ctx, []string{"doctor", "--json"}, "")
			if code != test.want {
				t.Fatalf("exit = %d, want %d", code, test.want)
			}
			if test.wantOutput != (stdout != "") {
				t.Fatalf("stdout = %q, want output = %v", stdout, test.wantOutput)
			}
		})
	}
}

func TestDoctorAbortAfterPartialChecksDoesNotRenderResult(t *testing.T) {
	for _, format := range []struct {
		name string
		args []string
	}{
		{name: "human", args: []string{"doctor", "--timeout", "1ms"}},
		{name: "json", args: []string{"doctor", "--json", "--timeout", "1ms"}},
	} {
		t.Run("timeout/"+format.name, func(t *testing.T) {
			deps := readyDoctorDependencies()
			deps.Version = func(ctx context.Context, _ string) (string, error) {
				<-ctx.Done()
				return "", ctx.Err()
			}
			installDoctorDependencies(t, deps)

			code, stdout, stderr := runCLI(t, format.args, "")
			if code != 7 || stdout != "" || !strings.Contains(stderr, "doctor timed out") {
				t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
		})

		t.Run("cancellation/"+format.name, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			deps := readyDoctorDependencies()
			deps.Version = func(ctx context.Context, _ string) (string, error) {
				cancel()
				<-ctx.Done()
				return "", ctx.Err()
			}
			installDoctorDependencies(t, deps)

			code, stdout, stderr := runCLIWithContext(t, parent, format.args, "")
			if code != 8 || stdout != "" || !strings.Contains(stderr, "doctor cancelled") {
				t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
		})
	}
}

func TestDoctorMissingExecutableKeepsReadinessExitWithoutCatalogProcess(t *testing.T) {
	deps := readyDoctorDependencies()
	deps.Lookup = func(string) (string, error) { return "", errors.New("missing") }
	factoryCalls := 0
	deps.CatalogFactory = func(string) pi.ModelCatalog {
		factoryCalls++
		return &fakeCatalog{models: []pi.ModelProjection{{Provider: "acme", ID: "model"}}}
	}
	installDoctorDependencies(t, deps)

	code, stdout, stderr := runCLI(t, []string{"doctor"}, "")
	if code != 3 || stderr != "" || factoryCalls != 0 {
		t.Fatalf("exit = %d, stderr = %q, factory calls = %d", code, stderr, factoryCalls)
	}
	want := []string{
		"pi-executable: failed - Pi executable is unavailable",
		"pi-version: failed - Pi version could not be checked",
		"config: ok - Pi-worker configuration is valid",
		"model-catalog: failed - Pi model catalog is unavailable",
		"default-model: failed - Configured default model is unavailable",
		"workspace: ok - Workspace is a git work tree; a run can report what it changed and check declared writes.",
		"ready: no",
	}
	got := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(got) != len(want) {
		t.Fatalf("output = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("output line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestDoctorDefaultTimeoutIs30Seconds(t *testing.T) {
	// This catches doctor accidentally inheriting run's 30-minute timeout.
	deadlineSeen := make(chan time.Duration, 1)
	deps := readyDoctorDependencies()
	deps.Catalog = &fakeCatalog{list: func(ctx context.Context, _ pi.CatalogRequest) ([]pi.ModelProjection, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("catalog context has no deadline")
		}
		deadlineSeen <- time.Until(deadline)
		return []pi.ModelProjection{{Provider: "acme", ID: "model"}}, nil
	}}
	installDoctorDependencies(t, deps)
	code, _, stderr := runCLI(t, []string{"doctor"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if remaining := <-deadlineSeen; remaining < 29*time.Second || remaining > 31*time.Second {
		t.Fatalf("timeout = %v, want about 30s", remaining)
	}
}

func TestDoctorRejectsInvalidFlags(t *testing.T) {
	// This catches accepting malformed input and starting an inspection anyway.
	for _, args := range [][]string{
		{"doctor", "--unknown"}, {"doctor", "--json", "--json"}, {"doctor", "--debug=true"}, {"doctor", "--timeout"}, {"doctor", "--timeout", "0s"}, {"doctor", "--timeout=bad"},
	} {
		t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
			code, stdout, stderr := runCLI(t, args, "")
			if code != 2 || stdout != "" || !strings.Contains(stderr, "pi-worker:") {
				t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
		})
	}
}

func TestDoctorRealFakePiUsesOnlyCatalogRequest(t *testing.T) {
	// This catches wiring doctor through the worker path, which would submit a
	// prompt or activate a model instead of performing a read-only catalog check.
	setupFakePiScript(t, &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"model"}]}`)}}},
	}})
	logPath := os.Getenv("FAKEPI_LOG")
	deps := readyDoctorDependencies()
	deps.Lookup = func(string) (string, error) { return fakePiBin, nil }
	deps.Version = func(context.Context, string) (string, error) { return "0.84.2", nil }
	deps.Catalog = pi.NewCatalog(fakePiBin)
	deps.Workspace = func() (string, error) { return t.TempDir(), nil }
	installDoctorDependencies(t, deps)

	code, stdout, stderr := runCLI(t, []string{"doctor", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	var result doctor.Result
	if err := json.Unmarshal([]byte(stdout), &result); err != nil || !result.Ready {
		t.Fatalf("JSON = %q, err = %v", stdout, err)
	}
	waitForRequestLog(t, logPath, "get_available_models")
	data, err := os.ReadFile(logPath)
	if err != nil || strings.Count(string(data), `"type":"get_available_models"`) != 1 || strings.Contains(string(data), `"type":"prompt"`) {
		t.Fatalf("requests = %q, err = %v", data, err)
	}
}

func expiredDoctorContext(t *testing.T, kind error) context.Context {
	t.Helper()
	if errors.Is(kind, context.DeadlineExceeded) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		t.Cleanup(cancel)
		<-ctx.Done()
		return ctx
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

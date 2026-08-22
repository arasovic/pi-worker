package doctor

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/config"
	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/run"
)

const workspaceWarningMessage = "Not a confirmed git work tree: a run here cannot report what it changed and cannot check declared writes. Workers run with your permissions in this directory. A git work tree enables both checks."
const workspaceUnreadableMessage = "Git work tree found but its state could not be read: a run here cannot report what it changed and cannot check declared writes."

const seededSecret = "seeded-credential-value"
const seededEnvironment = "seeded-environment-value"

func readyDependencies() Dependencies {
	return Dependencies{
		Lookup:  func(string) (string, error) { return "pi", nil },
		Version: func(context.Context, string) (string, error) { return "0.84.2", nil },
		LoadConfig: func() (config.Config, error) {
			return config.Config{SchemaVersion: 1, DefaultModel: "acme/model"}, nil
		},
		Catalog:   &catalogFake{models: []pi.ModelProjection{{Provider: "acme", ID: "model"}}},
		Workspace: func() (string, error) { return ".", nil },
		Inspect:   func(context.Context, string) (*run.GitState, error) { return &run.GitState{}, nil },
	}
}

func TestRunKeepsSixChecksInDeterministicOrder(t *testing.T) {
	result, err := Run(context.Background(), readyDependencies())
	if err != nil {
		t.Fatalf("Run error = %v", err)
	}
	want := []string{"pi-executable", "pi-version", "config", "model-catalog", "default-model", "workspace"}
	if len(result.Checks) != len(want) {
		t.Fatalf("checks = %#v", result.Checks)
	}
	for i, name := range want {
		if result.Checks[i].Name != name {
			t.Fatalf("checks[%d].Name = %q, want %q", i, result.Checks[i].Name, name)
		}
	}
}

func TestRunIsReadyWhenAllChecksPass(t *testing.T) {
	result, err := Run(context.Background(), readyDependencies())
	if err != nil || !result.Ready || result.SchemaVersion != 1 {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	for _, check := range result.Checks {
		if check.Status != CheckOK {
			t.Fatalf("check = %#v", check)
		}
	}
}

func TestRunReportsMissingExecutableAsFailed(t *testing.T) {
	deps := readyDependencies()
	deps.Lookup = func(string) (string, error) { return "", errors.New("missing " + seededEnvironment) }
	catalog := &catalogFake{models: []pi.ModelProjection{{Provider: "acme", ID: "model"}}}
	deps.Catalog = catalog
	result, err := Run(context.Background(), deps)
	if err != nil || result.Ready || len(result.Checks) != 6 || result.Checks[0].Status != CheckFailed || result.Checks[3].Status != CheckFailed || result.Checks[4].Status != CheckFailed {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	if catalog.calls != 0 {
		t.Fatalf("catalog calls = %d, want 0 when the executable is unavailable", catalog.calls)
	}
	assertRedacted(t, result)
}

func TestRunReportsValidUnverifiedVersionAsWarningAndReady(t *testing.T) {
	deps := readyDependencies()
	deps.Version = func(context.Context, string) (string, error) { return "0.99.0", nil }
	result, err := Run(context.Background(), deps)
	if err != nil || !result.Ready || result.Checks[1].Status != CheckWarning {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	if got, want := result.Checks[1].Message, "Pi version 0.99.0 is unverified; verified version is 0.84.2"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestRunReportsMalformedVersionAsFailed(t *testing.T) {
	deps := readyDependencies()
	deps.Version = func(context.Context, string) (string, error) { return "pi 0.84.2", nil }
	result, err := Run(context.Background(), deps)
	if err != nil || result.Ready || result.Checks[1].Status != CheckFailed {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
}

func TestRunUsesResolvedExecutableForVersionAndCatalog(t *testing.T) {
	deps := readyDependencies()
	const resolvedExecutable = "/tmp/resolved-pi"
	deps.Lookup = func(string) (string, error) { return resolvedExecutable, nil }
	deps.Version = func(_ context.Context, executable string) (string, error) {
		if executable != resolvedExecutable {
			t.Fatalf("version executable = %q, want %q", executable, resolvedExecutable)
		}
		return "0.84.2", nil
	}
	factoryCalls := 0
	deps.CatalogFactory = func(executable string) pi.ModelCatalog {
		factoryCalls++
		if executable != resolvedExecutable {
			t.Fatalf("catalog executable = %q, want %q", executable, resolvedExecutable)
		}
		return &catalogFake{models: []pi.ModelProjection{{Provider: "acme", ID: "model"}}}
	}
	result, err := Run(context.Background(), deps)
	if err != nil || !result.Ready || factoryCalls != 1 {
		t.Fatalf("result = %#v, err = %v, factory calls = %d", result, err, factoryCalls)
	}
}

func TestRunReportsMissingConfigAsWarning(t *testing.T) {
	deps := readyDependencies()
	deps.LoadConfig = func() (config.Config, error) { return config.Config{}, fs.ErrNotExist }
	result, err := Run(context.Background(), deps)
	if err != nil || !result.Ready || result.Checks[2].Status != CheckWarning || result.Checks[4].Status != CheckWarning {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
}

func TestRunNeverFlattensMalformedConfigToWarning(t *testing.T) {
	for _, cfg := range []config.Config{
		{},
		{SchemaVersion: 1, DefaultModel: "acme/model"},
	} {
		deps := readyDependencies()
		deps.LoadConfig = func() (config.Config, error) { return cfg, errors.New("malformed " + seededSecret) }
		result, err := Run(context.Background(), deps)
		if err != nil || result.Ready || result.Checks[2].Status != CheckFailed || result.Checks[4].Status != CheckFailed {
			t.Fatalf("config = %#v, result = %#v, err = %v", cfg, result, err)
		}
		assertRedacted(t, result)
	}
}

func TestRunReportsEmptyCatalogAsFailed(t *testing.T) {
	deps := readyDependencies()
	deps.Catalog = &catalogFake{}
	result, err := Run(context.Background(), deps)
	if err != nil || result.Ready || result.Checks[3].Status != CheckFailed {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
}

func TestRunChecksConfiguredDefaultAgainstCatalog(t *testing.T) {
	for _, test := range []struct {
		name   string
		models []pi.ModelProjection
		want   CheckStatus
	}{
		{"present", []pi.ModelProjection{{Provider: "acme", ID: "model"}}, CheckOK},
		{"absent", []pi.ModelProjection{{Provider: "acme", ID: "other"}}, CheckFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			deps := readyDependencies()
			deps.Catalog = &catalogFake{models: test.models}
			result, err := Run(context.Background(), deps)
			if err != nil || result.Checks[4].Status != test.want {
				t.Fatalf("result = %#v, err = %v", result, err)
			}
		})
	}
}

func TestRunReportsConfigStatusInDefaultModelCheck(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want CheckStatus
	}{
		{"missing", fs.ErrNotExist, CheckWarning},
		{"failure", errors.New("config " + seededEnvironment), CheckFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			deps := readyDependencies()
			deps.LoadConfig = func() (config.Config, error) { return config.Config{}, test.err }
			result, err := Run(context.Background(), deps)
			if err != nil || len(result.Checks) != 6 || result.SchemaVersion != 1 || result.Checks[4].Status != test.want {
				t.Fatalf("result = %#v, err = %v", result, err)
			}
			assertRedacted(t, result)
		})
	}
}

func TestRunWorkspaceWarnsWhenGitCannotConfirm(t *testing.T) {
	deps := readyDependencies()
	deps.Inspect = func(context.Context, string) (*run.GitState, error) { return nil, nil }
	result, err := Run(context.Background(), deps)
	if err != nil || !result.Ready {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	last := result.Checks[len(result.Checks)-1]
	if last.Name != "workspace" || last.Status != CheckWarning || last.Message != workspaceWarningMessage {
		t.Fatalf("last check = %#v", last)
	}
}

func TestRunWorkspaceOKWhenGitConfirmsWorkTree(t *testing.T) {
	deps := readyDependencies()
	deps.Inspect = func(context.Context, string) (*run.GitState, error) { return &run.GitState{}, nil }
	result, err := Run(context.Background(), deps)
	if err != nil || !result.Ready {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	last := result.Checks[len(result.Checks)-1]
	if last.Name != "workspace" || last.Status != CheckOK {
		t.Fatalf("last check = %#v", last)
	}
}

func TestRunWorkspaceWarnsWhenGitStateUnreadable(t *testing.T) {
	// The guard passed, so this is a work tree, but a later command
	// failed, so a run here can neither report what it changed nor check
	// declared writes.
	deps := readyDependencies()
	deps.Inspect = func(context.Context, string) (*run.GitState, error) { return nil, errors.New("git state") }
	result, err := Run(context.Background(), deps)
	if err != nil || !result.Ready {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	last := result.Checks[len(result.Checks)-1]
	if last.Name != "workspace" || last.Status != CheckWarning || last.Message != workspaceUnreadableMessage {
		t.Fatalf("last check = %#v", last)
	}
}

func TestRunWorkspaceWarnsWithoutAnInspector(t *testing.T) {
	// A Dependencies value built without an inspector must warn, not
	// panic: the check reaches for deps.Inspect, so the guard has to be
	// read before the call rather than alongside its result.
	deps := readyDependencies()
	deps.Inspect = nil
	result, err := Run(context.Background(), deps)
	if err != nil || !result.Ready {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	last := result.Checks[len(result.Checks)-1]
	if last.Name != "workspace" || last.Status != CheckWarning || last.Message != workspaceWarningMessage {
		t.Fatalf("last check = %#v", last)
	}
}

func TestRunWorkspaceWarnsWhenGettingWorkspaceFails(t *testing.T) {
	deps := readyDependencies()
	deps.Workspace = func() (string, error) { return "", errors.New("getwd" + seededEnvironment) }
	// A workspace failure also blocks the model catalog, which the runner
	// surfaces as an internal failure after the workspace check is recorded.
	result, err := Run(context.Background(), deps)
	if FailureKindOf(err) != FailureInternal {
		t.Fatalf("FailureKindOf(%v) = %q, want %q", err, FailureKindOf(err), FailureInternal)
	}
	last := result.Checks[len(result.Checks)-1]
	if last.Name != "workspace" || last.Status != CheckWarning || last.Message != workspaceWarningMessage {
		t.Fatalf("last check = %#v", last)
	}
	assertRedacted(t, result)
}

func TestRunClassifiesCatalogProtocolAsInternal(t *testing.T) {
	deps := readyDependencies()
	deps.Catalog = &catalogFake{err: &pi.ProtocolError{Message: "bad " + seededSecret}}
	_, err := Run(context.Background(), deps)
	if FailureKindOf(err) != FailureInternal {
		t.Fatalf("FailureKindOf(%v) = %q", err, FailureKindOf(err))
	}
}

func TestRunCancellationAndTimeoutTakePrecedence(t *testing.T) {
	for _, test := range []struct {
		name string
		ctx  context.Context
		want FailureKind
	}{
		{"timeout", timeoutContext(t), FailureTimeout},
		{"cancellation", cancelledContext(), FailureCancellation},
	} {
		t.Run(test.name, func(t *testing.T) {
			deps := readyDependencies()
			deps.Catalog = &catalogFake{err: &pi.ProtocolError{Message: "bad"}}
			_, err := Run(test.ctx, deps)
			if FailureKindOf(err) != test.want {
				t.Fatalf("FailureKindOf(%v) = %q, want %q", err, FailureKindOf(err), test.want)
			}
		})
	}
}

func TestRunCatalogSendsNoPromptRequest(t *testing.T) {
	catalog := &catalogFake{models: []pi.ModelProjection{{Provider: "acme", ID: "model"}}}
	deps := readyDependencies()
	deps.Catalog = catalog
	if _, err := Run(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	if catalog.prompts != 0 || catalog.calls != 1 {
		t.Fatalf("catalog calls = %d, prompts = %d", catalog.calls, catalog.prompts)
	}
}

type catalogFake struct {
	models  []pi.ModelProjection
	err     error
	calls   int
	prompts int
}

func (f *catalogFake) List(context.Context, pi.CatalogRequest) ([]pi.ModelProjection, error) {
	f.calls++
	return f.models, f.err
}

func assertRedacted(t *testing.T, result Result) {
	t.Helper()
	text := ""
	for _, check := range result.Checks {
		text += check.Message
	}
	if strings.Contains(text, seededSecret) || strings.Contains(text, seededEnvironment) {
		t.Fatalf("messages leak seeded value: %q", text)
	}
}

func timeoutContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	t.Cleanup(cancel)
	<-ctx.Done()
	return ctx
}

func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

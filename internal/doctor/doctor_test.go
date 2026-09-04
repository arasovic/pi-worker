package doctor

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/config"
	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/piversion"
	"github.com/arasovic/pi-worker/internal/run"
)

const seededSecret = "seeded-credential-value"
const seededEnvironment = "seeded-environment-value"

func readyDependencies() Dependencies {
	return Dependencies{
		Lookup:  func(string) (string, error) { return "pi", nil },
		Version: func(context.Context, string) (string, error) { return piversion.VerifiedVersion, nil },
		LoadConfig: func() (config.Config, error) {
			return config.Config{SchemaVersion: 1, DefaultModel: "acme/model"}, nil
		},
		Catalog:   &catalogFake{models: []pi.ModelProjection{{Provider: "acme", ID: "model"}}},
		Workspace: func() (string, error) { return ".", nil },
		GitInspector: &gitInspectorFake{
			state: &run.GitState{Head: "deadbeef", Branch: "main"},
		},
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
	if got, want := result.Checks[1].Message, "Pi version 0.99.0 is unverified; verified version is "+piversion.VerifiedVersion; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestRunReportsMalformedVersionAsFailed(t *testing.T) {
	deps := readyDependencies()
	deps.Version = func(context.Context, string) (string, error) { return "pi 0.84.1", nil }
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
		return piversion.VerifiedVersion, nil
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

func TestRunReportsDanglingConfigLinkAsFailed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks is not reliably available on Windows")
	}
	// The real user configuration document is a dangling final-component
	// link: doctor must report the config check failed — never the missing-
	// configuration warning meant for a genuinely absent file — and leave
	// the link untouched. loadUserConfig is the production read path, so
	// this pins the shared behavior exactly as DefaultDependencies wires it.
	missingTarget := filepath.Join(t.TempDir(), "not", "there", "pi-worker.json")
	path := installUserConfigPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(missingTarget, path); err != nil {
		t.Fatal(err)
	}

	deps := readyDependencies()
	deps.LoadConfig = loadUserConfig
	result, err := Run(context.Background(), deps)
	if err != nil {
		t.Fatalf("Run error = %v", err)
	}
	if result.Ready || result.Checks[2].Status != CheckFailed || result.Checks[4].Status != CheckFailed {
		t.Fatalf("result = %#v, want failed config and default-model checks", result)
	}
	// doctor keeps its failed-config message categorical — it never echoes
	// the underlying error — so the check must fail rather than read as the
	// missing-file warning that would leave the environment ready.
	if msg := result.Checks[2].Message; msg == "No pi-worker configuration found" {
		t.Fatalf("config message = %q, a dangling link must not read as a missing configuration", msg)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config symlink replaced = %v, %v", info, err)
	}
	if got, err := os.Readlink(path); err != nil || got != missingTarget {
		t.Fatalf("config symlink target = %q, %v; want %q", got, err, missingTarget)
	}
	if _, err := os.Stat(missingTarget); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat(%q) after doctor = %v, want fs.ErrNotExist", missingTarget, err)
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

func TestRunRejectsDefaultThatRunnableSelectorGrammarCannotAddress(t *testing.T) {
	const selector = "ac/me/model"
	model := pi.ModelProjection{Provider: "ac/me", ID: "model"}
	provider, id, ok := strings.Cut(selector, "/")
	if !ok || (provider == model.Provider && id == model.ID) {
		t.Fatalf("first-slash selector grammar unexpectedly addresses %#v", model)
	}

	deps := readyDependencies()
	deps.LoadConfig = func() (config.Config, error) {
		return config.Config{SchemaVersion: 1, DefaultModel: selector}, nil
	}
	deps.Catalog = &catalogFake{models: []pi.ModelProjection{model}}
	result, err := Run(context.Background(), deps)
	if err != nil {
		t.Fatalf("Run error = %v", err)
	}
	if result.Ready || result.Checks[4].Status != CheckFailed {
		t.Fatalf("result = %#v, want an unavailable default model", result)
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

// gitInspectorFake records every Inspect call and returns the configured
// state and error, so the doctor workspace check can be pinned without
// running git.
type gitInspectorFake struct {
	state *run.GitState
	err   error
	dir   string
	calls int
}

func (g *gitInspectorFake) Inspect(_ context.Context, dir string) (*run.GitState, error) {
	g.calls++
	g.dir = dir
	return g.state, g.err
}

func TestRunWorkspaceCheckConfirmsGitWorkTree(t *testing.T) {
	deps := readyDependencies()
	inspector := &gitInspectorFake{state: &run.GitState{Head: "deadbeef"}}
	deps.GitInspector = inspector
	result, err := Run(context.Background(), deps)
	if err != nil || !result.Ready || result.Checks[5].Status != CheckOK || !strings.Contains(result.Checks[5].Message, "confirmed git work tree") {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	if inspector.calls != 1 || inspector.dir != "." {
		t.Fatalf("inspector calls = %d, dir = %q, want exactly one Inspect of the workspace %q", inspector.calls, inspector.dir, ".")
	}
}

func TestRunWorkspaceCheckUnconfirmedWorkTreeIsWarningAndReady(t *testing.T) {
	deps := readyDependencies()
	// A nil state with no error is Inspect's collapse of every way a work
	// tree cannot be confirmed: not inside one, git missing, or a
	// transient guard failure.
	deps.GitInspector = &gitInspectorFake{}
	result, err := Run(context.Background(), deps)
	if err != nil || !result.Ready || result.Checks[5].Status != CheckWarning {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	for _, phrase := range []string{"not inside a confirmed git work tree", "change manifest", "declared-writes check"} {
		if !strings.Contains(result.Checks[5].Message, phrase) {
			t.Fatalf("message = %q, want it to state what is lost: %q", result.Checks[5].Message, phrase)
		}
	}
}

func TestRunWorkspaceCheckErrorAfterGuardIsWarningAndReady(t *testing.T) {
	deps := readyDependencies()
	// An error can only come from a command after the guard, so the work
	// tree was confirmed; the message must never read as an unconfirmed
	// work tree.
	deps.GitInspector = &gitInspectorFake{state: &run.GitState{}, err: errors.New("git status: exit status 1")}
	result, err := Run(context.Background(), deps)
	if err != nil || !result.Ready || result.Checks[5].Status != CheckWarning {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	if msg := result.Checks[5].Message; !strings.Contains(msg, "inside a confirmed git work tree") || strings.Contains(msg, "not inside") {
		t.Fatalf("message = %q, want a confirmed work tree with a measurement problem", msg)
	}
}

func TestRunWorkspaceCheckWithoutInspectorIsWarningAndReady(t *testing.T) {
	deps := readyDependencies()
	deps.GitInspector = nil
	result, err := Run(context.Background(), deps)
	if err != nil || !result.Ready || result.Checks[5].Status != CheckWarning || len(result.Checks) != 6 {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
}

func TestRunWorkspaceCheckDoesNotAskWithoutAWorkspace(t *testing.T) {
	deps := readyDependencies()
	deps.Workspace = nil
	inspector := &gitInspectorFake{state: &run.GitState{Head: "deadbeef"}}
	deps.GitInspector = inspector
	result, err := Run(context.Background(), deps)
	if err != nil {
		// The catalog cannot run without a workspace and doctor inherits
		// its existing handling; what this test pins is that the
		// workspace check still reports and never asks git.
		t.Logf("Run error = %v (inherited catalog handling)", err)
	}
	if len(result.Checks) != 6 || result.Checks[5].Status != CheckWarning || !strings.Contains(result.Checks[5].Message, "could not be determined") {
		t.Fatalf("result = %#v", result)
	}
	if inspector.calls != 0 {
		t.Fatalf("inspector calls = %d, want 0: no workspace to ask about", inspector.calls)
	}
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

// installUserConfigPath redirects the operating-system user configuration
// directory to an isolated temp location and returns the pi-worker
// config.json path there, so loadUserConfig — the production read path —
// reads an entirely hermetic document instead of the real home directory.
func installUserConfigPath(t *testing.T) string {
	t.Helper()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("AppData", filepath.Join(t.TempDir(), "AppData"))
	case "darwin":
		t.Setenv("HOME", t.TempDir())
	default:
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	}
	path, err := config.UserPath()
	if err != nil {
		t.Fatalf("UserPath(): %v", err)
	}
	return path
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

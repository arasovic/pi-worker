package doctor

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"pi-worker/internal/config"
	"pi-worker/internal/pi"
)

const seededSecret = "seeded-credential-value"
const seededEnvironment = "seeded-environment-value"

func readyDependencies() Dependencies {
	return Dependencies{
		Lookup:  func(string) (string, error) { return "pi", nil },
		Version: func(context.Context, string) (string, error) { return "0.84.1", nil },
		LoadConfig: func() (config.Config, error) {
			return config.Config{SchemaVersion: 1, DefaultModel: "acme/model"}, nil
		},
		Catalog:   &catalogFake{models: []pi.ModelProjection{{Provider: "acme", ID: "model"}}},
		Workspace: func() (string, error) { return ".", nil },
		Home:      func() (string, error) { return "/home/test", nil },
		Stat:      func(string) (fs.FileInfo, error) { return regularFileInfo{}, nil },
	}
}

func TestRunKeepsSixChecksInDeterministicOrder(t *testing.T) {
	result, err := Run(context.Background(), readyDependencies())
	if err != nil {
		t.Fatalf("Run error = %v", err)
	}
	want := []string{"pi-executable", "pi-version", "config", "model-catalog", "default-model", "global-skill"}
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
	result, err := Run(context.Background(), deps)
	if err != nil || result.Ready || result.Checks[0].Status != CheckFailed {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	assertRedacted(t, result)
}

func TestRunReportsUnsupportedVersionAsFailed(t *testing.T) {
	deps := readyDependencies()
	deps.Version = func(context.Context, string) (string, error) { return "0.99.0", nil }
	result, err := Run(context.Background(), deps)
	if err != nil || result.Ready || result.Checks[1].Status != CheckFailed {
		t.Fatalf("result = %#v, err = %v", result, err)
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
	deps := readyDependencies()
	deps.LoadConfig = func() (config.Config, error) { return config.Config{}, errors.New("malformed " + seededSecret) }
	result, err := Run(context.Background(), deps)
	if err != nil || result.Ready || result.Checks[2].Status != CheckFailed {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	assertRedacted(t, result)
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

func TestRunReportsMissingSkillAndStatFailure(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want CheckStatus
	}{
		{"missing", fs.ErrNotExist, CheckWarning},
		{"failure", errors.New("stat " + seededEnvironment), CheckFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			deps := readyDependencies()
			deps.Stat = func(string) (fs.FileInfo, error) { return nil, test.err }
			result, err := Run(context.Background(), deps)
			if err != nil || result.Checks[5].Status != test.want {
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

type regularFileInfo struct{}

func (regularFileInfo) Name() string       { return "SKILL.md" }
func (regularFileInfo) Size() int64        { return 0 }
func (regularFileInfo) Mode() fs.FileMode  { return 0 }
func (regularFileInfo) ModTime() time.Time { return time.Time{} }
func (regularFileInfo) IsDir() bool        { return false }
func (regularFileInfo) Sys() any           { return nil }

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

var _ = os.ErrNotExist

// Package doctor inspects local pi-worker readiness without changing state.
package doctor

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"

	"pi-worker/internal/config"
	"pi-worker/internal/pi"
)

type CheckStatus string

const (
	CheckOK      CheckStatus = "ok"
	CheckWarning CheckStatus = "warning"
	CheckFailed  CheckStatus = "failed"
)

type Check struct {
	Name    string      `json:"name"`
	Status  CheckStatus `json:"status"`
	Message string      `json:"message"`
}

type Result struct {
	SchemaVersion int     `json:"schemaVersion"`
	Ready         bool    `json:"ready"`
	Checks        []Check `json:"checks"`
}

type FailureKind string

const (
	FailureReadiness    FailureKind = "readiness"
	FailureInternal     FailureKind = "internal"
	FailureTimeout      FailureKind = "timeout"
	FailureCancellation FailureKind = "cancellation"
)

type failure struct{ kind FailureKind }

func (f *failure) Error() string { return string(f.kind) }

func FailureKindOf(err error) FailureKind {
	var failed *failure
	if errors.As(err, &failed) {
		return failed.kind
	}
	return FailureInternal
}

type Dependencies struct {
	Lookup         func(string) (string, error)
	Version        func(context.Context, string) (string, error)
	LoadConfig     func() (config.Config, error)
	CatalogFactory func(string) pi.ModelCatalog
	Catalog        pi.ModelCatalog
	Workspace      func() (string, error)
	Home           func() (string, error)
	Stat           func(string) (fs.FileInfo, error)
	Debug          *pi.DebugSink
}

func Run(ctx context.Context, deps Dependencies) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, contextFailure(err)
	}
	result := Result{SchemaVersion: 1, Ready: true, Checks: make([]Check, 0, 6)}
	executable, executableErr := deps.Lookup("pi")
	if executableErr != nil {
		add(&result, "pi-executable", CheckFailed, "Pi executable is unavailable")
	} else {
		add(&result, "pi-executable", CheckOK, "Pi executable found")
	}
	if executableErr != nil {
		add(&result, "pi-version", CheckFailed, "Pi version could not be checked")
	} else if version, err := deps.Version(ctx, executable); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, contextFailure(ctxErr)
		}
		add(&result, "pi-version", CheckFailed, "Pi version could not be checked")
	} else if version != "0.84.1" {
		add(&result, "pi-version", CheckFailed, "Pi version is unsupported")
	} else {
		add(&result, "pi-version", CheckOK, "Pi version 0.84.1 is supported")
	}

	cfg, configErr := deps.LoadConfig()
	configMissing := errors.Is(configErr, fs.ErrNotExist)
	switch {
	case configMissing:
		add(&result, "config", CheckWarning, "No pi-worker configuration found")
	case configErr != nil:
		add(&result, "config", CheckFailed, "Pi-worker configuration is invalid")
	default:
		add(&result, "config", CheckOK, "Pi-worker configuration is valid")
	}

	workspace, workspaceErr := deps.Workspace()
	var models []pi.ModelProjection
	var catalogErr error
	if executableErr != nil {
		catalogErr = &pi.ReadinessError{Message: "pi executable is unavailable"}
	} else {
		models, catalogErr = catalog(ctx, deps, executable, workspace, workspaceErr)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, contextFailure(ctxErr)
	}
	if catalogErr != nil || len(models) == 0 {
		add(&result, "model-catalog", CheckFailed, "Pi model catalog is unavailable")
	} else {
		add(&result, "model-catalog", CheckOK, "Pi model catalog is available")
	}

	switch {
	case configErr != nil && !configMissing:
		add(&result, "default-model", CheckFailed, "Configured default model could not be checked")
	case configMissing || cfg.DefaultModel == "":
		add(&result, "default-model", CheckWarning, "No default model is configured")
	case catalogErr != nil || !contains(models, cfg.DefaultModel):
		add(&result, "default-model", CheckFailed, "Configured default model is unavailable")
	default:
		add(&result, "default-model", CheckOK, "Configured default model is available")
	}

	home, homeErr := deps.Home()
	if homeErr != nil {
		add(&result, "global-skill", CheckFailed, "Global pi-worker skill could not be checked")
	} else if info, err := deps.Stat(filepath.Join(home, ".agents", "skills", "pi-worker", "SKILL.md")); errors.Is(err, fs.ErrNotExist) {
		add(&result, "global-skill", CheckWarning, "Global pi-worker skill is not installed")
	} else if err != nil || !info.Mode().IsRegular() {
		add(&result, "global-skill", CheckFailed, "Global pi-worker skill is invalid")
	} else {
		add(&result, "global-skill", CheckOK, "Global pi-worker skill is installed")
	}

	if catalogErr != nil {
		var readiness *pi.ReadinessError
		if !errors.As(catalogErr, &readiness) {
			return result, &failure{kind: FailureInternal}
		}
	}
	return result, nil
}

func add(result *Result, name string, status CheckStatus, message string) {
	result.Checks = append(result.Checks, Check{Name: name, Status: status, Message: message})
	if status == CheckFailed {
		result.Ready = false
	}
}

func catalog(ctx context.Context, deps Dependencies, executable, workspace string, workspaceErr error) ([]pi.ModelProjection, error) {
	if workspaceErr != nil {
		return nil, errors.New("catalog unavailable")
	}
	catalog := deps.Catalog
	if deps.CatalogFactory != nil {
		catalog = deps.CatalogFactory(executable)
	}
	if catalog == nil {
		return nil, errors.New("catalog unavailable")
	}
	return catalog.List(ctx, pi.CatalogRequest{Workspace: workspace, Debug: deps.Debug})
}

func contains(models []pi.ModelProjection, selector string) bool {
	for _, model := range models {
		if model.Provider+"/"+model.ID == selector {
			return true
		}
	}
	return false
}

func contextFailure(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &failure{kind: FailureTimeout}
	}
	return &failure{kind: FailureCancellation}
}

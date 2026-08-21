// Package doctor inspects local pi-worker readiness without changing state.
package doctor

import (
	"context"
	"errors"
	"io/fs"

	"github.com/arasovic/pi-worker/internal/config"
	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/piversion"
	"github.com/arasovic/pi-worker/internal/run"
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
	Inspect        func(context.Context, string) (*run.GitState, error)
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
	} else {
		switch piversion.Classify(version).Status {
		case piversion.StatusVerified:
			add(&result, "pi-version", CheckOK, "Pi version "+piversion.VerifiedVersion+" is supported")
		case piversion.StatusUnverified:
			add(&result, "pi-version", CheckWarning, "Pi version "+version+" is unverified; verified version is "+piversion.VerifiedVersion)
		default:
			add(&result, "pi-version", CheckFailed, "Pi version is unsupported")
		}
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

	// The work-tree question is answered by the same guard a run uses, so
	// the two surfaces can never disagree about the same directory: a nil
	// state with no error is Inspect saying it could not confirm a work
	// tree, an error means the guard passed and a later command failed, so
	// it is a work tree but its state cannot be read, and a non-nil state
	// is a work tree read successfully. Without a workspace or an inspector
	// there is nothing to ask, and asking anyway would run git in an
	// unspecified directory.
	status := CheckWarning
	message := "Not a confirmed git work tree: a run here cannot report what it changed and cannot check declared writes. Workers run with your permissions in this directory. A git work tree enables both checks."
	if workspaceErr == nil && deps.Inspect != nil {
		state, inspectErr := deps.Inspect(ctx, workspace)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, contextFailure(ctxErr)
		}
		switch {
		case state != nil:
			// Guard passed and the state was read: a run can report what it
			// changed and check declared writes.
			status, message = CheckOK, "Workspace is a git work tree; a run can report what it changed and check declared writes."
		case inspectErr != nil:
			// Guard passed, so this is a work tree, but a command after the
			// guard failed, so a run here loses both checks.
			message = "Git work tree found but its state could not be read: a run here cannot report what it changed and cannot check declared writes."
		}
	}
	add(&result, "workspace", status, message)

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

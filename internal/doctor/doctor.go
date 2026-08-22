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
	GitInspector   run.GitInspector
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

	var workspace string
	var workspaceErr error
	if deps.Workspace == nil {
		// A caller that leaves the workspace resolver unset gets the same
		// outcome as one whose resolver fails: there is no workspace to
		// ask about, so the catalog cannot run in it and nothing else may
		// probe it either.
		workspaceErr = errors.New("workspace resolver unavailable")
	} else {
		workspace, workspaceErr = deps.Workspace()
	}
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

	// The workspace check is advisory and appended last. It asks the same
	// guard a run asks — GitInspector.Inspect — so doctor and run can
	// never disagree about the same directory. A warning never makes the
	// environment unready: a run outside a confirmed git work tree still
	// runs, but its change manifest is omitted with the
	// work-tree-unconfirmed reason and its declared-writes check is
	// skipped. The dependency and the workspace are both optional; when
	// either is absent the check reports that it could not ask rather
	// than running git in a directory it does not have. It runs even
	// when the catalog could not be checked: the report is the point,
	// and a missing workspace is not a reason to skip the check that
	// exists to say the workspace is missing.
	switch {
	case workspaceErr != nil:
		add(&result, "workspace", CheckWarning, "Workspace could not be determined; git state was not checked")
	case deps.GitInspector == nil:
		add(&result, "workspace", CheckWarning, "No git inspector is configured; git state was not checked")
	default:
		state, err := deps.GitInspector.Inspect(ctx, workspace)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, contextFailure(ctxErr)
		}
		switch {
		case err != nil:
			// The guard runs first, so an error can only come from a
			// command after it: the work tree was confirmed. This is not
			// the "not a work tree" case and must not read as one.
			add(&result, "workspace", CheckWarning, "Workspace is inside a confirmed git work tree, but its git state could not be fully measured; a run there would omit its change manifest and skip its declared-writes check")
		case state != nil:
			add(&result, "workspace", CheckOK, "Workspace is inside a confirmed git work tree")
		default:
			add(&result, "workspace", CheckWarning, "Workspace is not inside a confirmed git work tree; a run there would omit its change manifest and skip its declared-writes check")
		}
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

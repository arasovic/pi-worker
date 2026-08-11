package pi

import (
	"context"
	"slices"
	"strings"
)

// CatalogRequest describes one catalog query: the workspace the read-only
// catalog process runs in, and the optional run-level debug sink.
type CatalogRequest struct {
	Workspace string
	Debug     *DebugSink
}

// ModelCatalog lists the available-model catalog of the host pi executable.
type ModelCatalog interface {
	List(context.Context, CatalogRequest) ([]ModelProjection, error)
}

// catalog launches the host pi executable in RPC mode with a read-only tool
// profile and issues exactly one get_available_models request.
type catalog struct {
	executable string
}

// NewCatalog returns a ModelCatalog that launches the given host pi
// executable.
func NewCatalog(executable string) ModelCatalog {
	return &catalog{executable: executable}
}

// List starts a read-only catalog process and issues exactly one
// get_available_models request: it never activates a model, submits a
// prompt, or issues any other RPC. The returned catalog is sorted by
// provider then id. An explicitly empty catalog is a readiness failure (Pi
// is configured but unusable); malformed data is preserved as a protocol
// error. The process is always closed, so cancellation or timeout
// terminates the child and removes its session directory.
func (c *catalog) List(ctx context.Context, req CatalogRequest) ([]ModelProjection, error) {
	proc, err := newCatalogProcess(c.executable, req.Workspace)
	if err != nil {
		return nil, err
	}
	defer proc.Close()

	if err := proc.Start(ctx); err != nil {
		return nil, err
	}
	// A catalog query is a single process with no worker id of its own, so
	// its debug lines carry the direct-caller label.
	client := NewClient(proc.Stdin(), proc.Stdout(), nil, req.Debug.Worker(1))
	models, err := client.GetAvailableModels(ctx)
	if err != nil {
		// The kill callback can close the stream before Client observes the
		// caller's cancellation. The caller's completed context is primary.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if len(models) == 0 {
		return nil, &ReadinessError{Message: "get_available_models returned an empty catalog; verify Pi compatibility and provider login"}
	}
	slices.SortFunc(models, func(a, b ModelProjection) int {
		if a.Provider != b.Provider {
			return strings.Compare(a.Provider, b.Provider)
		}
		return strings.Compare(a.ID, b.ID)
	})
	return models, nil
}

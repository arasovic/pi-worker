package cli

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/pi"
)

type fakeCatalog struct {
	models []pi.ModelProjection
	err    error
	list   func(context.Context, pi.CatalogRequest) ([]pi.ModelProjection, error)
}

func (f *fakeCatalog) List(ctx context.Context, req pi.CatalogRequest) ([]pi.ModelProjection, error) {
	if f.list != nil {
		return f.list(ctx, req)
	}
	return append([]pi.ModelProjection(nil), f.models...), f.err
}

func installFakeCatalog(t *testing.T, catalog pi.ModelCatalog) {
	t.Helper()
	original := newCatalog
	newCatalog = func() pi.ModelCatalog { return catalog }
	t.Cleanup(func() { newCatalog = original })
}

func decodeModelsOutput(t *testing.T, text string) modelsOutput {
	t.Helper()
	var output modelsOutput
	if err := json.Unmarshal([]byte(text), &output); err != nil {
		t.Fatalf("decode models JSON %q: %v", text, err)
	}
	return output
}

func TestModelsHumanOutputUsesSortedExactSelectors(t *testing.T) {
	// This catches human output that is unsorted or omits the exact
	// provider/id selector needed for a subsequent run command.
	installFakeCatalog(t, &fakeCatalog{models: []pi.ModelProjection{
		{Provider: "zeta", ID: "last"}, {Provider: "acme", ID: "z"}, {Provider: "acme", ID: "a"},
	}})

	code, stdout, stderr := runCLI(t, []string{"models"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if stdout != "acme/a\nacme/z\nzeta/last\n" {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestModelsJSONIsOneSortedDocument(t *testing.T) {
	// This catches JSON output that emits multiple documents or exposes Pi's
	// incoming order rather than deterministic exact selectors.
	installFakeCatalog(t, &fakeCatalog{models: []pi.ModelProjection{
		{Provider: "zeta", ID: "last"}, {Provider: "acme", ID: "a"},
	}})

	code, stdout, stderr := runCLI(t, []string{"models", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
		t.Fatalf("stdout contains multiple documents: %q", stdout)
	}
	output := decodeModelsOutput(t, stdout)
	if output.SchemaVersion != 1 {
		t.Fatalf("schemaVersion = %d, want 1", output.SchemaVersion)
	}
	want := []modelOutput{{Provider: "acme", ID: "a", Selector: "acme/a"}, {Provider: "zeta", ID: "last", Selector: "zeta/last"}}
	if len(output.Models) != len(want) {
		t.Fatalf("models = %#v, want %#v", output.Models, want)
	}
	for i := range want {
		if output.Models[i] != want[i] {
			t.Fatalf("models[%d] = %#v, want %#v", i, output.Models[i], want[i])
		}
	}
}

func TestModelsDebugStaysOnStderr(t *testing.T) {
	// This catches diagnostics leaking into stdout and corrupting --json.
	installFakeCatalog(t, &fakeCatalog{list: func(_ context.Context, req pi.CatalogRequest) ([]pi.ModelProjection, error) {
		req.Debug.Worker(1).Log("rpc=get_available_models", "status=completed")
		return []pi.ModelProjection{{Provider: "acme", ID: "a"}}, nil
	}})

	code, stdout, stderr := runCLI(t, []string{"models", "--json", "--debug"}, "")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	_ = decodeModelsOutput(t, stdout)
	if !strings.Contains(stderr, "[pi-worker +") || strings.Contains(stdout, "[pi-worker +") {
		t.Fatalf("stdout = %q, stderr = %q", stdout, stderr)
	}
}

func TestModelsTimeoutExits7(t *testing.T) {
	// This catches a catalog timeout being incorrectly reported as readiness
	// or internal failure.
	installFakeCatalog(t, &fakeCatalog{list: func(ctx context.Context, _ pi.CatalogRequest) ([]pi.ModelProjection, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}})

	code, stdout, stderr := runCLI(t, []string{"models", "--timeout", "1ms"}, "")
	if code != 7 || stdout != "" || !strings.Contains(stderr, "timed out") {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestModelsRejectsUnknownAndDuplicateFlags(t *testing.T) {
	// This catches accepting an unsupported catalog flag or silently letting a
	// duplicate flag override a prior value.
	for _, args := range [][]string{{"models", "--unknown"}, {"models", "--json", "--json"}, {"models", "--timeout", "1s", "--timeout", "2s"}} {
		t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
			code, stdout, stderr := runCLI(t, args, "")
			if code != 2 || stdout != "" || !strings.Contains(stderr, "pi-worker:") {
				t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
		})
	}
}

func TestModelsEmptyCatalogExits3(t *testing.T) {
	// This catches presenting a known-empty catalog as a successful result.
	installFakeCatalog(t, &fakeCatalog{err: &pi.ReadinessError{Message: "empty catalog"}})

	code, stdout, stderr := runCLI(t, []string{"models", "--json"}, "")
	if code != 3 || stdout != "" || !strings.Contains(stderr, "empty catalog") {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestModelsMissingExecutableExits3(t *testing.T) {
	// This catches mapping a catalog startup failure to internal exit 9
	// instead of the user-actionable readiness exit.
	installFakeCatalog(t, pi.NewCatalog(filepath.Join(t.TempDir(), "missing-pi")))

	code, stdout, stderr := runCLI(t, []string{"models"}, "")
	if code != 3 || stdout != "" || !strings.Contains(stderr, "pi not ready") {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestModelsProtocolErrorExits9(t *testing.T) {
	// This catches malformed Pi data being downgraded to readiness failure.
	installFakeCatalog(t, &fakeCatalog{err: &pi.ProtocolError{Message: "bad catalog"}})

	code, stdout, stderr := runCLI(t, []string{"models"}, "")
	if code != 9 || stdout != "" || !strings.Contains(stderr, "protocol error") {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestModelsMalformedCatalogSelectorExits9WithoutJSON(t *testing.T) {
	// This catches publishing a selector that cannot be passed back to run.
	installFakeCatalog(t, &fakeCatalog{models: []pi.ModelProjection{{Provider: "ac/me", ID: "model"}}})

	code, stdout, stderr := runCLI(t, []string{"models", "--json"}, "")
	if code != 9 || stdout != "" || !strings.Contains(stderr, "protocol error") {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestModelsCancellationExits8(t *testing.T) {
	// This catches a caller cancellation being mistaken for a timeout or an
	// internal catalog failure.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	installFakeCatalog(t, &fakeCatalog{err: errors.New("catalog should not run")})

	code, stdout, stderr := runCLIWithContext(t, ctx, []string{"models"}, "")
	if code != 8 || stdout != "" || !strings.Contains(stderr, "cancelled") {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestModelsDefaultTimeoutIs30Seconds(t *testing.T) {
	// This catches an omitted timeout using the run command's 30-minute
	// default instead of the catalog command's bounded 30-second default.
	deadlineSeen := make(chan time.Duration, 1)
	installFakeCatalog(t, &fakeCatalog{list: func(ctx context.Context, _ pi.CatalogRequest) ([]pi.ModelProjection, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatalf("catalog context has no deadline")
		}
		deadlineSeen <- time.Until(deadline)
		return []pi.ModelProjection{{Provider: "acme", ID: "a"}}, nil
	}})

	code, _, stderr := runCLI(t, []string{"models"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if remaining := <-deadlineSeen; remaining < 29*time.Second || remaining > 31*time.Second {
		t.Fatalf("timeout = %v, want about 30s", remaining)
	}
}

package pi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"pi-worker/internal/testutil/fakepi/script"
)

func TestCatalogListsSortedModelsWithOnlyCatalogRequest(t *testing.T) {
	// This catches a catalog implementation that emits a prompt, activates a
	// model, or preserves Pi's nondeterministic catalog order.
	logPath := setupFakePiEnv(t, &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"zeta","id":"last"},{"provider":"acme","id":"z"},{"provider":"acme","id":"a"}]}`)}},
		},
	}})

	models, err := NewCatalog(fakePiBin).List(context.Background(), CatalogRequest{Workspace: t.TempDir()})
	if err != nil {
		t.Fatalf("list catalog: %v", err)
	}
	want := []ModelProjection{{Provider: "acme", ID: "a"}, {Provider: "acme", ID: "z"}, {Provider: "zeta", ID: "last"}}
	if !slices.Equal(models, want) {
		t.Fatalf("models = %#v, want %#v", models, want)
	}
	if got := waitRequestLog(t, logPath, 1); !slices.Equal(got, []string{"get_available_models"}) {
		t.Fatalf("request log = %v, want exactly [get_available_models]", got)
	}
}

func TestCatalogRejectsExplicitEmptyCatalogAsReadiness(t *testing.T) {
	// This catches accepting an explicit empty catalog, which would make a
	// configured-but-unavailable Pi look usable to config and doctor.
	setupFakePiEnv(t, &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[]}`)}},
		},
	}})

	_, err := NewCatalog(fakePiBin).List(context.Background(), CatalogRequest{Workspace: t.TempDir()})
	var readiness *ReadinessError
	if !errors.As(err, &readiness) {
		t.Fatalf("error = %v, want ReadinessError", err)
	}
}

func TestCatalogClassifiesStartFailureAsReadiness(t *testing.T) {
	// This catches leaking a host executable startup failure as an internal
	// error, which makes read-only catalog consumers report exit 9.
	missingExecutable := filepath.Join(t.TempDir(), "missing-pi")

	_, err := NewCatalog(missingExecutable).List(context.Background(), CatalogRequest{Workspace: t.TempDir()})
	var readiness *ReadinessError
	if !errors.As(err, &readiness) {
		t.Fatalf("error = %v, want ReadinessError", err)
	}
}

func TestCatalogStartFailurePreservesCompletedContext(t *testing.T) {
	// This catches classifying a completed caller context as readiness when
	// it must keep the timeout/cancellation exit mapping.
	for _, test := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{name: "cancelled", ctx: cancelledContext(), want: context.Canceled},
		{name: "deadline exceeded", ctx: expiredContext(), want: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewCatalog(filepath.Join(t.TempDir(), "missing-pi")).List(test.ctx, CatalogRequest{Workspace: t.TempDir()})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func expiredContext() context.Context {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	cancel()
	return ctx
}

func TestCatalogRejectsMalformedDataAsProtocolError(t *testing.T) {
	// This catches treating malformed catalog data as an empty/ready catalog.
	setupFakePiEnv(t, &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":"not-an-array"}`)}},
		},
	}})

	_, err := NewCatalog(fakePiBin).List(context.Background(), CatalogRequest{Workspace: t.TempDir()})
	var protocol *ProtocolError
	if !errors.As(err, &protocol) {
		t.Fatalf("error = %v, want ProtocolError", err)
	}
}

func TestCatalogPreservesMissingOrNullModelsAsProtocolError(t *testing.T) {
	// This catches Catalog reinterpreting Client's malformed missing/null
	// models contract as an empty catalog or a readiness failure.
	for _, data := range []string{`{}`, `{"models":null}`} {
		t.Run(data, func(t *testing.T) {
			setupFakePiEnv(t, &script.Script{Triggers: map[string][]script.Step{
				"get_available_models": {
					{Response: &script.Response{Success: true, Data: json.RawMessage(data)}},
				},
			}})

			_, err := NewCatalog(fakePiBin).List(context.Background(), CatalogRequest{Workspace: t.TempDir()})
			var protocol *ProtocolError
			if !errors.As(err, &protocol) {
				t.Fatalf("error = %v, want ProtocolError", err)
			}
		})
	}
}

func TestCatalogTimeoutCleansUpProcessAndSession(t *testing.T) {
	// This catches a timed-out catalog query that leaves the child or its
	// private session directory behind.
	setupFakePiEnv(t, &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {{SleepMS: 10_000}},
	}})
	metaPath := filepath.Join(t.TempDir(), "meta.json")
	t.Setenv("FAKEPI_META", metaPath)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := NewCatalog(fakePiBin).List(ctx, CatalogRequest{Workspace: t.TempDir()})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want DeadlineExceeded", err)
	}
	sessionDir := sessionDirFromMeta(t, metaPath)
	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Fatalf("session directory left behind after timeout: %v", err)
	}
}

func TestCatalogCancellationAfterResponseWinsAndCleansUp(t *testing.T) {
	// This catches returning a successful catalog after the caller cancelled
	// immediately after Pi's response arrived.
	setupFakePiEnv(t, &script.Script{Triggers: map[string][]script.Step{
		"get_available_models": {
			{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"a"}]}`)}},
		},
	}})
	metaPath := filepath.Join(t.TempDir(), "meta.json")
	t.Setenv("FAKEPI_META", metaPath)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	debug := piDebugCancelAfterCatalogResponse{cancel: cancel}

	models, err := NewCatalog(fakePiBin).List(ctx, CatalogRequest{
		Workspace: t.TempDir(),
		Debug:     NewDebugSink(&debug),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled; models = %#v", err, models)
	}
	sessionDir := sessionDirFromMeta(t, metaPath)
	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Fatalf("session directory left behind after cancellation: %v", err)
	}
}

type piDebugCancelAfterCatalogResponse struct {
	cancel func()
	once   bool
}

func (w *piDebugCancelAfterCatalogResponse) Write(data []byte) (int, error) {
	if !w.once && strings.Contains(string(data), "rpc=get_available_models status=completed") {
		w.once = true
		w.cancel()
	}
	return len(data), nil
}

var _ io.Writer = (*piDebugCancelAfterCatalogResponse)(nil)

package pi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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

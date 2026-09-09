package livepi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/arasovic/pi-worker/internal/piversion"
)

func TestDecideEmptyModel(t *testing.T) {
	result := Decide("", func(string) (string, error) { return "/usr/bin/pi", nil },
		func(context.Context, string) (string, error) { return piversion.VerifiedVersion, nil })
	if result.Ready {
		t.Fatal("empty model must not be ready")
	}
	if !strings.Contains(result.Reason, ModelEnvVar) || !strings.Contains(result.Reason, "model") {
		t.Fatalf("reason %q does not name the missing model", result.Reason)
	}
}

func TestDecideLookupFails(t *testing.T) {
	result := Decide("some-model", func(string) (string, error) { return "", errors.New("not found") },
		func(context.Context, string) (string, error) { return piversion.VerifiedVersion, nil })
	if result.Ready {
		t.Fatal("failed lookup must not be ready")
	}
	if !strings.Contains(result.Reason, ExecutableName) {
		t.Fatalf("reason %q does not name the missing executable", result.Reason)
	}
}

func TestDecideUnverifiedVersion(t *testing.T) {
	result := Decide("some-model", func(string) (string, error) { return "/usr/bin/pi", nil },
		func(context.Context, string) (string, error) { return "0.1.2", nil })
	if result.Ready {
		t.Fatal("unverified version must not be ready")
	}
	if !strings.Contains(result.Reason, "0.1.2") || !strings.Contains(result.Reason, piversion.VerifiedVersion) {
		t.Fatalf("reason %q does not name found and verified versions", result.Reason)
	}
}

func TestDecideInvalidVersionOutput(t *testing.T) {
	result := Decide("some-model", func(string) (string, error) { return "/usr/bin/pi", nil },
		func(context.Context, string) (string, error) { return "pi 0.85.1 (build 1234)", nil })
	if result.Ready {
		t.Fatal("unparseable version output must not be ready")
	}
}

func TestDecideReady(t *testing.T) {
	const model = "some-model"
	result := Decide(model, func(string) (string, error) { return "/usr/bin/pi", nil },
		func(context.Context, string) (string, error) { return piversion.VerifiedVersion + "\n", nil })
	if !result.Ready {
		t.Fatalf("expected ready, got reason %q", result.Reason)
	}
	if result.Executable != "/usr/bin/pi" {
		t.Fatalf("executable = %q, want /usr/bin/pi", result.Executable)
	}
	if result.Model != model {
		t.Fatalf("model = %q, want %q", result.Model, model)
	}
}

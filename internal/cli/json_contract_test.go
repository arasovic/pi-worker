package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/arasovic/pi-worker/internal/config"
	"github.com/arasovic/pi-worker/internal/pi"
	"github.com/arasovic/pi-worker/internal/skillinstall"
)

func decodeJSONObject(t *testing.T, text string) map[string]any {
	t.Helper()
	if strings.TrimSpace(text) == "" || strings.Count(strings.TrimSpace(text), "\n") != 0 {
		t.Fatalf("stdout is not one JSON document: %q", text)
	}
	var document map[string]any
	if err := json.Unmarshal([]byte(text), &document); err != nil {
		t.Fatalf("decode JSON %q: %v", text, err)
	}
	return document
}

func assertExactJSONKeys(t *testing.T, document map[string]any, keys ...string) {
	t.Helper()
	actual := make([]string, 0, len(document))
	for key := range document {
		actual = append(actual, key)
	}
	sort.Strings(actual)
	sort.Strings(keys)
	if len(actual) != len(keys) {
		t.Fatalf("JSON keys = %v, want %v", actual, keys)
	}
	for i := range keys {
		if actual[i] != keys[i] {
			t.Fatalf("JSON keys = %v, want %v", actual, keys)
		}
	}
}

func requireJSONArray(t *testing.T, value any, name string) []any {
	t.Helper()
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("%s = %#v, want non-null array", name, value)
	}
	return items
}

func TestPublicJSONDocumentShapes(t *testing.T) {
	t.Run("version", func(t *testing.T) {
		code, stdout, stderr := runCLI(t, []string{"version", "--json"}, "")
		if code != 0 || stderr != "" {
			t.Fatalf("version = (%d, %q, %q)", code, stdout, stderr)
		}
		assertExactJSONKeys(t, decodeJSONObject(t, stdout), "schemaVersion", "version", "commit", "buildDate")
	})

	t.Run("models", func(t *testing.T) {
		installFakeCatalog(t, &fakeCatalog{models: []pi.ModelProjection{{Provider: "acme", ID: "model"}}})
		code, stdout, stderr := runCLI(t, []string{"models", "--json"}, "")
		if code != 0 || stderr != "" {
			t.Fatalf("models = (%d, %q, %q)", code, stdout, stderr)
		}
		document := decodeJSONObject(t, stdout)
		assertExactJSONKeys(t, document, "schemaVersion", "models")
		models := requireJSONArray(t, document["models"], "models")
		assertExactJSONKeys(t, models[0].(map[string]any), "provider", "id", "selector")
	})

	t.Run("doctor", func(t *testing.T) {
		installDoctorDependencies(t, readyDoctorDependencies())
		code, stdout, stderr := runCLI(t, []string{"doctor", "--json"}, "")
		if code != 0 || stderr != "" {
			t.Fatalf("doctor = (%d, %q, %q)", code, stdout, stderr)
		}
		document := decodeJSONObject(t, stdout)
		assertExactJSONKeys(t, document, "schemaVersion", "ready", "checks")
		checks := requireJSONArray(t, document["checks"], "checks")
		assertExactJSONKeys(t, checks[0].(map[string]any), "name", "status", "message")
	})

	t.Run("config show", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := config.Save(path, config.Config{SchemaVersion: 1}); err != nil {
			t.Fatal(err)
		}
		installConfigPath(t, path)
		code, stdout, stderr := runCLI(t, []string{"config", "show", "--json"}, "")
		if code != 0 || stderr != "" {
			t.Fatalf("config show = (%d, %q, %q)", code, stdout, stderr)
		}
		assertExactJSONKeys(t, decodeJSONObject(t, stdout), "schemaVersion", "defaultModel")
	})

	t.Run("skill receipt path", func(t *testing.T) {
		installSkillReceiptPath(t, filepath.Join(t.TempDir(), "skill-install.json"))
		code, stdout, stderr := runCLI(t, []string{"skill", "receipt-path", "--json"}, "")
		if code != 0 || stderr != "" {
			t.Fatalf("skill receipt-path = (%d, %q, %q)", code, stdout, stderr)
		}
		assertExactJSONKeys(t, decodeJSONObject(t, stdout), "schemaVersion", "receiptPath")
	})

	t.Run("skill status", func(t *testing.T) {
		installSkillReceiptPath(t, filepath.Join(t.TempDir(), "skill-install.json"))
		code, stdout, stderr := runCLI(t, []string{"skill", "status", "--json"}, "")
		if code != 3 || stderr != "" {
			t.Fatalf("skill status = (%d, %q, %q)", code, stdout, stderr)
		}
		document := decodeJSONObject(t, stdout)
		assertExactJSONKeys(t, document, "schemaVersion", "receiptPath", "status", "verifiedTargets", "trackedTargets", "affectedTargets", "recovery", "externalInspection")
		requireJSONArray(t, document["verifiedTargets"], "verifiedTargets")
		requireJSONArray(t, document["trackedTargets"], "trackedTargets")
		requireJSONArray(t, document["affectedTargets"], "affectedTargets")
		requireJSONArray(t, document["recovery"], "recovery")
		external := document["externalInspection"].(map[string]any)
		assertExactJSONKeys(t, external, "state", "targets")
		requireJSONArray(t, external["targets"], "externalInspection.targets")
	})

	t.Run("run", func(t *testing.T) {
		installFakeWorker(t, pi.WorkerResult{
			Model:                  "acme/model",
			RequestedThinkingLevel: pi.ThinkingMax,
			ThinkingLevel:          pi.ThinkingHigh,
			ThinkingFallback:       true,
			Warning:                "fallback",
			Explanation:            "done",
			Status:                 pi.StatusCompleted,
		})
		code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/model", "--thinking", "max", "--task", "work", "--json"}, "")
		if code != 0 || stderr != "pi-worker: worker 1: fallback\n" {
			t.Fatalf("run = (%d, %q, %q)", code, stdout, stderr)
		}
		document := decodeJSONObject(t, stdout)
		assertExactJSONKeys(t, document, "changes", "schemaVersion", "status", "workers")
		workers := requireJSONArray(t, document["workers"], "workers")
		assertExactJSONKeys(t, workers[0].(map[string]any), "model", "requestedThinkingLevel", "thinkingLevel", "thinkingFallback", "warning", "explanation", "status")
	})
}

func TestDurableReceiptJSONShape(t *testing.T) {
	receipt := skillinstall.Receipt{
		SchemaVersion:    skillinstall.SchemaVersion,
		InstallerVersion: "0.1.0",
		SkillsVersion:    skillinstall.PinnedSkillsVersion,
		Outcome:          skillinstall.OutcomeSkipped,
		Targets:          []skillinstall.Target{},
		AffectedTargets:  []skillinstall.AffectedTarget{},
		Recovery:         []string{},
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	document := decodeJSONObject(t, string(data))
	assertExactJSONKeys(t, document, "schemaVersion", "installerVersion", "skillsVersion", "outcome", "targets", "affectedTargets", "recovery")
	requireJSONArray(t, document["targets"], "targets")
	requireJSONArray(t, document["affectedTargets"], "affectedTargets")
	requireJSONArray(t, document["recovery"], "recovery")

	path := filepath.Join(t.TempDir(), "receipt.json")
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := skillinstall.Load(path); err != nil {
		t.Fatalf("load durable receipt: %v", err)
	}
}

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

		original := inspectSkillReceipt
		inspectSkillReceipt = func(string) (skillinstall.Inspection, error) {
			return skillinstall.Inspection{
				SchemaVersion:    skillinstall.SchemaVersion,
				ReceiptPath:      "/receipt.json",
				Status:           skillinstall.StatusStale,
				InstallerVersion: "0.5.0",
				ProgramVersion:   "0.6.0",
				VerifiedTargets:  []string{},
				TrackedTargets:   []string{},
				AffectedTargets:  []skillinstall.AffectedTarget{},
				Recovery:         []string{skillinstall.SafeRecoveryCommand},
				ExternalInspection: skillinstall.ExternalInspection{
					State:   skillinstall.ExternalInspectionUnavailable,
					Targets: []skillinstall.ExternalTarget{},
				},
			}, nil
		}
		t.Cleanup(func() { inspectSkillReceipt = original })
		code, stdout, stderr = runCLI(t, []string{"skill", "status", "--json"}, "")
		if code != 3 || stderr != "" {
			t.Fatalf("skill status with versions = (%d, %q, %q)", code, stdout, stderr)
		}
		versioned := decodeJSONObject(t, stdout)
		assertExactJSONKeys(t, versioned, "schemaVersion", "receiptPath", "status", "installerVersion", "programVersion", "verifiedTargets", "trackedTargets", "affectedTargets", "recovery", "externalInspection")
	})

	t.Run("runs list", func(t *testing.T) {
		dir := t.TempDir()
		withRunlogDir(t, dir)
		writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/ws", 1, true, "completed", "")
		code, stdout, stderr := runCLI(t, []string{"runs", "list", "--json"}, "")
		if code != 0 || stderr != "" {
			t.Fatalf("runs list = (%d, %q, %q)", code, stdout, stderr)
		}
		document := decodeJSONObject(t, stdout)
		assertExactJSONKeys(t, document, "schemaVersion", "runs")
		runs := requireJSONArray(t, document["runs"], "runs")
		assertExactJSONKeys(t, runs[0].(map[string]any), "runId", "startedAt", "workspace", "tasks", "outcome", "path")
	})

	t.Run("runs prune", func(t *testing.T) {
		dir := t.TempDir()
		withRunlogDir(t, dir)
		writeListRecord(t, dir, "20260830T101500Z-1", deadPID, "2026-08-30T10:15:00Z", "/ws", 1, true, "completed", "")
		code, stdout, stderr := runCLI(t, []string{"runs", "prune", "--keep", "0", "--yes", "--json"}, "")
		if code != 0 || stderr != "" {
			t.Fatalf("runs prune = (%d, %q, %q)", code, stdout, stderr)
		}
		document := decodeJSONObject(t, stdout)
		assertExactJSONKeys(t, document, "schemaVersion", "deleted", "keptNewest", "keptRunning", "keptUnreadable")
		requireJSONArray(t, document["deleted"], "deleted")
		requireJSONArray(t, document["keptRunning"], "keptRunning")
		requireJSONArray(t, document["keptUnreadable"], "keptUnreadable")
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
		assertExactJSONKeys(t, document, "changes", "outcome", "schemaVersion", "status", "workers")
		if document["outcome"] != "completed" {
			t.Fatalf("outcome = %v, want completed", document["outcome"])
		}
		workers := requireJSONArray(t, document["workers"], "workers")
		assertExactJSONKeys(t, workers[0].(map[string]any), "model", "requestedThinkingLevel", "thinkingLevel", "thinkingFallback", "warning", "explanation", "status")
	})
}

func TestRunJSONGitObjectExactKeysWithBranchAndStash(t *testing.T) {
	repo := newGitWorkspace(t)
	fake := installFakeWorker(t, pi.WorkerResult{Model: "acme/model", Status: pi.StatusCompleted, Explanation: "done"})
	fake.runHook = func() {
		gitRun(t, repo, "checkout", "-q", "-b", "feature")
		if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("stashed\n"), 0o644); err != nil {
			t.Errorf("write file: %v", err)
			return
		}
		gitRun(t, repo, "stash", "push", "-q", "-m", "saved")
	}
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/model", "--task", "work", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("run = (%d, %q, %q)", code, stdout, stderr)
	}
	document := decodeJSONObject(t, stdout)
	assertExactJSONKeys(t, document, "changes", "git", "outcome", "schemaVersion", "status", "workers")
	git, ok := document["git"].(map[string]any)
	if !ok {
		t.Fatalf("git = %#v, want object", document["git"])
	}
	assertExactJSONKeys(t, git, "before", "after", "stash")
	before, ok := git["before"].(map[string]any)
	if !ok {
		t.Fatalf("git.before = %#v, want object", git["before"])
	}
	after, ok := git["after"].(map[string]any)
	if !ok {
		t.Fatalf("git.after = %#v, want object", git["after"])
	}
	assertExactJSONKeys(t, before, "head", "branch", "dirty", "stashes")
	assertExactJSONKeys(t, after, "head", "branch", "dirty", "stashes")
	if branch, ok := before["branch"].(string); !ok || branch == "" {
		t.Fatalf("git.before.branch = %v, want the fixture's initial branch", before["branch"])
	}
	if after["branch"] != "feature" {
		t.Fatalf("git.after.branch = %v, want feature", after["branch"])
	}
	if before["stashes"] != float64(0) {
		t.Fatalf("git.before.stashes = %v, want 0", before["stashes"])
	}
	if after["stashes"] != float64(1) {
		t.Fatalf("git.after.stashes = %v, want 1", after["stashes"])
	}
	stash, ok := git["stash"].(map[string]any)
	if !ok {
		t.Fatalf("git.stash = %#v, want object", git["stash"])
	}
	assertExactJSONKeys(t, stash, "added")
	if len(requireJSONArray(t, stash["added"], "git.stash.added")) != 1 {
		t.Fatalf("git.stash.added = %v, want one entry", stash["added"])
	}
}

func TestRunJSONGitObjectExactKeysOmitsDetachedBranch(t *testing.T) {
	repo := newGitWorkspace(t)
	gitRun(t, repo, "checkout", "--detach", "HEAD")
	fake := installFakeWorker(t, pi.WorkerResult{Model: "acme/model", Status: pi.StatusCompleted, Explanation: "done"})
	fake.runHook = func() {
		if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("two\n"), 0o644); err != nil {
			t.Errorf("write file: %v", err)
			return
		}
		gitRun(t, repo, "add", "file.txt")
		gitRun(t, repo, "commit", "-q", "-m", "detached")
	}
	code, stdout, stderr := runCLI(t, []string{"run", "--model", "acme/model", "--task", "work", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("run = (%d, %q, %q)", code, stdout, stderr)
	}
	document := decodeJSONObject(t, stdout)
	assertExactJSONKeys(t, document, "changes", "git", "outcome", "schemaVersion", "status", "workers")
	git, ok := document["git"].(map[string]any)
	if !ok {
		t.Fatalf("git = %#v, want object", document["git"])
	}
	assertExactJSONKeys(t, git, "before", "after")
	before, ok := git["before"].(map[string]any)
	if !ok {
		t.Fatalf("git.before = %#v, want object", git["before"])
	}
	after, ok := git["after"].(map[string]any)
	if !ok {
		t.Fatalf("git.after = %#v, want object", git["after"])
	}
	assertExactJSONKeys(t, before, "head", "dirty", "stashes")
	assertExactJSONKeys(t, after, "head", "dirty", "stashes")
	if _, present := before["branch"]; present {
		t.Fatalf("git.before unexpectedly carries branch: %v", before)
	}
	if _, present := after["branch"]; present {
		t.Fatalf("git.after unexpectedly carries branch: %v", after)
	}
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

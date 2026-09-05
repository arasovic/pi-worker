package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/arasovic/pi-worker/internal/config"
	"github.com/arasovic/pi-worker/internal/pi"
)

type countingCatalog struct {
	models []pi.ModelProjection
	err    error
	calls  int
	req    pi.CatalogRequest
}

type cancellingCatalog struct {
	cancel context.CancelFunc
	models []pi.ModelProjection
}

func (c *cancellingCatalog) List(_ context.Context, _ pi.CatalogRequest) ([]pi.ModelProjection, error) {
	c.cancel()
	return append([]pi.ModelProjection(nil), c.models...), nil
}

func (c *countingCatalog) List(_ context.Context, req pi.CatalogRequest) ([]pi.ModelProjection, error) {
	c.calls++
	c.req = req
	return append([]pi.ModelProjection(nil), c.models...), c.err
}

func installConfigPath(t *testing.T, path string) {
	t.Helper()
	original := userConfigPath
	userConfigPath = func() (string, error) { return path, nil }
	t.Cleanup(func() { userConfigPath = original })
}

// installCLIConfigPath redirects the real user-config path seam to a
// hermetic location and returns the config.json path there.
func installCLIConfigPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	installConfigPath(t, path)
	return path
}

func runCLIReader(t *testing.T, args []string, stdin io.Reader) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Main(args, stdin, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestConfigShowHumanOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Save(path, config.Config{SchemaVersion: 2, DefaultModel: "acme/model", MaxModelWorkers: 3}); err != nil {
		t.Fatal(err)
	}
	installConfigPath(t, path)

	code, stdout, stderr := runCLI(t, []string{"config", "show"}, "")
	if code != 0 || stdout != "default-model: acme/model\nmax-model-workers: 3\n" || stderr != "" {
		t.Fatalf("config show = (%d, %q, %q)", code, stdout, stderr)
	}
}

func TestConfigShowJSONIsSingleDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Save(path, config.Config{SchemaVersion: 2, MaxModelWorkers: 3}); err != nil {
		t.Fatal(err)
	}
	installConfigPath(t, path)

	code, stdout, stderr := runCLI(t, []string{"config", "show", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("config show --json = (%d, %q, %q)", code, stdout, stderr)
	}
	var got config.Config
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	if got != (config.Config{SchemaVersion: 2, MaxModelWorkers: 3}) || !bytes.HasSuffix([]byte(stdout), []byte("\n")) {
		t.Fatalf("JSON = %q, decoded %#v", stdout, got)
	}
}

func TestConfigShowReportsMissingMalformedAndDanglingLinkConfig(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		installConfigPath(t, path)

		code, stdout, stderr := runCLI(t, []string{"config", "show"}, "")
		if code != 0 || stdout != "default-model: \nmax-model-workers: 3\n" || stderr != "" {
			t.Fatalf("config show missing = (%d, %q, %q)", code, stdout, stderr)
		}

		code, stdout, stderr = runCLI(t, []string{"config", "show", "--json"}, "")
		if code != 0 || stdout != "{\"schemaVersion\":2,\"defaultModel\":\"\",\"maxModelWorkers\":3}\n" || stderr != "" {
			t.Fatalf("config show missing JSON = (%d, %q, %q)", code, stdout, stderr)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{"schemaVersion":1,"unexpected":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		installConfigPath(t, path)
		code, _, stderr := runCLI(t, []string{"config", "show"}, "")
		if code != 9 || stderr == "" {
			t.Fatalf("config show malformed = (%d, %q)", code, stderr)
		}
	})
	t.Run("dangling link", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("creating symlinks is not reliably available on Windows")
		}
		// A dangling final-component link is not a missing configuration: the
		// path holds a broken link a person must repair, and reading through
		// it must fail clearly instead of reporting an empty default. The
		// link itself is left exactly as it was.
		missingTarget := filepath.Join(t.TempDir(), "not", "there", "pi-worker.json")
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.Symlink(missingTarget, path); err != nil {
			t.Fatal(err)
		}
		installConfigPath(t, path)

		for _, args := range [][]string{
			{"config", "show"},
			{"config", "show", "--json"},
		} {
			code, stdout, stderr := runCLI(t, args, "")
			if code != 9 || stdout != "" || !strings.Contains(stderr, "symbolic link") {
				t.Fatalf("config show dangling link = (%d, %q, %q), want a clear dangling-link failure", code, stdout, stderr)
			}
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("config symlink replaced = %v, %v", info, err)
		}
		if got, err := os.Readlink(path); err != nil || got != missingTarget {
			t.Fatalf("config symlink target = %q, %v; want %q", got, err, missingTarget)
		}
	})
}

func TestConfigShowReadsThroughSymlinkedConfigPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks is not reliably available on Windows")
	}
	// Reads keep resolving the link: show reports the target's document even
	// though config set refuses to write through the same path.
	target := filepath.Join(t.TempDir(), "pi-worker.json")
	if err := os.WriteFile(target, []byte("{\"schemaVersion\":1,\"defaultModel\":\"acme/linked\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	installConfigPath(t, path)

	code, stdout, stderr := runCLI(t, []string{"config", "show"}, "")
	if code != 0 || stdout != "default-model: acme/linked\nmax-model-workers: 3\n" || stderr != "" {
		t.Fatalf("config show through a symlink = (%d, %q, %q)", code, stdout, stderr)
	}
}

func TestConfigSetRefusesSymlinkedConfigPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks is not reliably available on Windows")
	}
	// The dotfiles arrangement from the report: config.json links to a file
	// elsewhere that holds the current value. The catalog offers the model, so
	// the refusal is the write guard's, not the catalog's.
	target := filepath.Join(t.TempDir(), "dotfiles", "pi-worker.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	before := "{\"schemaVersion\":1,\"defaultModel\":\"acme/old\"}\n"
	if err := os.WriteFile(target, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	installConfigPath(t, path)
	installFakeCatalog(t, &countingCatalog{models: []pi.ModelProjection{{Provider: "acme", ID: "model"}}})

	code, stdout, stderr := runCLI(t, []string{"config", "set", "default-model", "acme/model"}, "")
	if code != 9 || stdout != "" || !strings.Contains(stderr, "symbolic link") {
		t.Fatalf("config set over a symlink = (%d, %q, %q)", code, stdout, stderr)
	}
	// The refusal came before anything was modified: the link is still a link
	// and the target still holds the old value.
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config symlink replaced = %v, %v", info, err)
	}
	after, err := os.ReadFile(target)
	if err != nil || string(after) != before {
		t.Fatalf("config target changed = %q, %v", after, err)
	}
}

func TestConfigSetValidatesExactSyntaxAndLiveCatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	installConfigPath(t, path)
	catalog := &countingCatalog{models: []pi.ModelProjection{{Provider: "acme", ID: "model"}}}
	installFakeCatalog(t, catalog)

	code, stdout, stderr := runCLI(t, []string{"config", "set", "default-model", "acme/model", "--timeout", "1s"}, "")
	if code != 0 || stdout != "default-model: acme/model\n" || stderr != "" {
		t.Fatalf("config set = (%d, %q, %q)", code, stdout, stderr)
	}
	if catalog.calls != 1 {
		t.Fatalf("catalog calls = %d, want 1", catalog.calls)
	}
	got, err := config.Load(path)
	if err != nil || got != (config.Config{SchemaVersion: 2, DefaultModel: "acme/model", MaxModelWorkers: 3}) {
		t.Fatalf("saved config = %#v, %v", got, err)
	}
}

func TestConfigSetRejectsInvalidSyntaxBeforeCatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	installConfigPath(t, path)
	catalog := &countingCatalog{}
	installFakeCatalog(t, catalog)

	code, _, stderr := runCLI(t, []string{"config", "set", "default-model", "acme"}, "")
	if code != 2 || stderr == "" || catalog.calls != 0 {
		t.Fatalf("config set invalid = (%d, %q), calls=%d", code, stderr, catalog.calls)
	}
}

func TestConfigSetUnavailableDoesNotChangeConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	before := config.Config{SchemaVersion: 2, DefaultModel: "acme/old", MaxModelWorkers: 3}
	if err := config.Save(path, before); err != nil {
		t.Fatal(err)
	}
	installConfigPath(t, path)
	installFakeCatalog(t, &countingCatalog{models: []pi.ModelProjection{{Provider: "acme", ID: "other"}}})

	code, _, stderr := runCLI(t, []string{"config", "set", "default-model", "acme/model"}, "")
	if code != 3 || stderr == "" {
		t.Fatalf("config set unavailable = (%d, %q)", code, stderr)
	}
	got, err := config.Load(path)
	if err != nil || got != before {
		t.Fatalf("config after unavailable set = %#v, %v", got, err)
	}
}

func TestConfigSetCatalogOfferedColonIdIsSaved(t *testing.T) {
	// A routing-provider catalog offers "acme/model:free". The name rule
	// accepts its shape, the catalog contains the name, so the default is
	// written exactly as named.
	path := filepath.Join(t.TempDir(), "config.json")
	installConfigPath(t, path)
	catalog := &countingCatalog{models: []pi.ModelProjection{{Provider: "acme", ID: "model:free"}}}
	installFakeCatalog(t, catalog)

	code, stdout, stderr := runCLI(t, []string{"config", "set", "default-model", "acme/model:free"}, "")
	if code != 0 || stdout != "default-model: acme/model:free\n" || stderr != "" {
		t.Fatalf("config set = (%d, %q, %q)", code, stdout, stderr)
	}
	got, err := config.Load(path)
	if err != nil || got != (config.Config{SchemaVersion: 2, DefaultModel: "acme/model:free", MaxModelWorkers: 3}) {
		t.Fatalf("saved config = %#v, %v", got, err)
	}
}

func TestConfigSetInventedColonNameFailsCatalogMembership(t *testing.T) {
	// An invented name carrying a colon passes the shape rule but is not
	// in the catalog: the catalog-membership check refuses it with the
	// not-in-the-available-catalog answer (exit 3), never a name-format
	// answer (exit 2).
	path := filepath.Join(t.TempDir(), "config.json")
	before := config.Config{SchemaVersion: 2, DefaultModel: "acme/old", MaxModelWorkers: 3}
	if err := config.Save(path, before); err != nil {
		t.Fatal(err)
	}
	installConfigPath(t, path)
	installFakeCatalog(t, &countingCatalog{models: []pi.ModelProjection{{Provider: "acme", ID: "model"}}})

	code, _, stderr := runCLI(t, []string{"config", "set", "default-model", "acme/model:free"}, "")
	if code != 3 || !strings.Contains(stderr, "not in the available catalog") {
		t.Fatalf("config set = (%d, %q), want the catalog-membership answer", code, stderr)
	}
	got, err := config.Load(path)
	if err != nil || got != before {
		t.Fatalf("config after refused set = %#v, %v", got, err)
	}
}

func TestConfigSetCancellationAfterCatalogDoesNotChangeConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	before := config.Config{SchemaVersion: 2, DefaultModel: "acme/old", MaxModelWorkers: 3}
	if err := config.Save(path, before); err != nil {
		t.Fatal(err)
	}
	installConfigPath(t, path)
	ctx, cancel := context.WithCancel(context.Background())
	installFakeCatalog(t, &cancellingCatalog{
		cancel: cancel,
		models: []pi.ModelProjection{{Provider: "acme", ID: "model"}},
	})
	var stdout, stderr bytes.Buffer

	code := mainWithContext(ctx, []string{"config", "set", "default-model", "acme/model"}, strings.NewReader(""), &stdout, &stderr)

	if code != 8 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "cancelled") {
		t.Fatalf("cancelled config set = (%d, %q, %q)", code, stdout.String(), stderr.String())
	}
	got, err := config.Load(path)
	if err != nil || got != before {
		t.Fatalf("config after cancelled set = %#v, %v", got, err)
	}
}

func TestConfigSetDebugStaysOnStderrAndCallsCatalogOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	installConfigPath(t, path)
	catalog := &countingCatalog{models: []pi.ModelProjection{{Provider: "acme", ID: "model"}}}
	installFakeCatalog(t, catalog)

	code, stdout, stderr := runCLI(t, []string{"config", "set", "default-model", "acme/model", "--debug"}, "")
	if code != 0 || stdout != "default-model: acme/model\n" || stderr != "" || catalog.calls != 1 || catalog.req.Debug == nil {
		t.Fatalf("config set --debug = (%d, %q, %q), calls=%d debug=%v", code, stdout, stderr, catalog.calls, catalog.req.Debug != nil)
	}
}

func TestMalformedConfigKeepsRunInputErrorsDistinctAndExplicitModelsDoNotReadIt(t *testing.T) {
	// A malformed durable document is an internal failure, not an argv
	// mistake. The explicit-model cases also pin the precedence rule: the
	// config path is not even resolved when every task already has a model.
	malformed := []byte(`{"schemaVersion":1,"unknown":true}`)
	empty := []byte(`{"schemaVersion":1}`)
	for _, test := range []struct {
		name       string
		args       []string
		configData []byte
		wantCode   int
		wantUsage  bool
		wantJSON   bool
		wantConfig int
	}{
		{name: "config show", args: []string{"config", "show"}, configData: malformed, wantCode: 9, wantConfig: 1},
		{name: "config show json", args: []string{"config", "show", "--json"}, configData: malformed, wantCode: 9, wantConfig: 1},
		{name: "run malformed default", args: []string{"run", "--task", "work"}, configData: malformed, wantCode: 9, wantConfig: 1},
		{name: "run malformed default json", args: []string{"run", "--task", "work", "--json"}, configData: malformed, wantCode: 9, wantConfig: 1},
		{name: "run missing default", args: []string{"run", "--task", "work"}, configData: empty, wantCode: 2, wantUsage: true, wantConfig: 1},
		{name: "run explicit", args: []string{"run", "--model", "acme/explicit", "--task", "work"}, configData: malformed, wantCode: 0, wantConfig: 0},
		{name: "run explicit json", args: []string{"run", "--model", "acme/explicit", "--task", "work", "--json"}, configData: malformed, wantCode: 0, wantJSON: true, wantConfig: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if test.configData != nil {
				if err := os.WriteFile(path, test.configData, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			configCalls := 0
			original := userConfigPath
			userConfigPath = func() (string, error) {
				configCalls++
				return path, nil
			}
			t.Cleanup(func() { userConfigPath = original })
			fake := installFakeWorker(t, pi.WorkerResult{Model: "acme/explicit", Status: pi.StatusCompleted, Explanation: "done"})

			code, stdout, stderr := runCLI(t, test.args, "")
			if code != test.wantCode {
				t.Fatalf("exit = %d, want %d; stdout=%q stderr=%q", code, test.wantCode, stdout, stderr)
			}
			if configCalls != test.wantConfig {
				t.Fatalf("config path resolved %d times, want %d", configCalls, test.wantConfig)
			}
			if test.wantCode == 9 {
				if stdout != "" || strings.Contains(stderr, "usage:") || !strings.Contains(stderr, "unknown field") {
					t.Fatalf("malformed config = (%q, %q), want no stdout, no usage, and the config error", stdout, stderr)
				}
				return
			}
			if test.wantUsage {
				if stdout != "" || !strings.Contains(stderr, "usage:") || !strings.Contains(stderr, "missing required flag --model") {
					t.Fatalf("missing default = (%q, %q), want usage and missing-model error", stdout, stderr)
				}
				return
			}
			if stderr != "" || fake.callCount() != 1 {
				t.Fatalf("explicit model = (%d, %q, %q), worker calls=%d", code, stdout, stderr, fake.callCount())
			}
			if test.wantJSON {
				_ = decodeJSONObject(t, stdout)
			} else if !strings.Contains(stdout, "worker 1: done") {
				t.Fatalf("explicit human stdout = %q", stdout)
			}
		})
	}
}

func TestRunModelExplicitNeverReadsMalformedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	before := []byte(`{"schemaVersion":1,"unknown":true}`)
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	installConfigPath(t, path)
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted, Explanation: "done"})

	code, _, stderr := runCLI(t, []string{"run", "--model", "acme/explicit", "--task", "work"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("run explicit = (%d, %q)", code, stderr)
	}
	if req, ok := fake.requestForWorker(1); !ok || req.Model != "acme/explicit" {
		t.Fatalf("worker request = %#v, present=%v", req, ok)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("config rewritten: %q, %v", after, err)
	}
}

func TestRunModelUsesSavedDefaultWhenOmitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Save(path, config.Config{SchemaVersion: 2, DefaultModel: "acme/default", MaxModelWorkers: 3}); err != nil {
		t.Fatal(err)
	}
	installConfigPath(t, path)
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted, Explanation: "done"})

	code, _, stderr := runCLI(t, []string{"run", "--task", "work"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("run default = (%d, %q)", code, stderr)
	}
	if req, ok := fake.requestForWorker(1); !ok || req.Model != "acme/default" {
		t.Fatalf("worker request = %#v, present=%v", req, ok)
	}
}

func TestRunModelDanglingConfigLinkFailsClearlyWithoutLaunchingOrTouchingLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks is not reliably available on Windows")
	}
	// A run needing the configured default reads through a dangling final
	// link: it must fail clearly — exit 9 like an invalid config, never the
	// missing-model usage error (2) meant for a genuinely absent file — and
	// launch nothing. The link must stay a link to the same target, and the
	// target must never be created.
	missingTarget := filepath.Join(t.TempDir(), "not", "there", "pi-worker.json")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.Symlink(missingTarget, path); err != nil {
		t.Fatal(err)
	}
	installConfigPath(t, path)
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted, Explanation: "done"})

	code, stdout, stderr := runCLI(t, []string{"run", "--task", "work"}, "")
	if code != 9 || stdout != "" || !strings.Contains(stderr, "symbolic link") {
		t.Fatalf("run dangling link = (%d, %q, %q), want a clear dangling-link failure", code, stdout, stderr)
	}
	if fake.callCount() != 0 {
		t.Fatalf("run dangling link launched %d workers", fake.callCount())
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config symlink replaced = %v, %v", info, err)
	}
	if got, err := os.Readlink(path); err != nil || got != missingTarget {
		t.Fatalf("config symlink target = %q, %v; want %q", got, err, missingTarget)
	}
	if _, err := os.Stat(missingTarget); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat(%q) after failed run = %v, want fs.ErrNotExist", missingTarget, err)
	}
}

func TestRunModelDefaultAppliesToEveryTaskWithoutItsOwn(t *testing.T) {
	// With neither a task nor a run --model, the configured defaultModel
	// applies to every task on a multi-task run, not only to the first.
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Save(path, config.Config{SchemaVersion: 2, DefaultModel: "acme/default", MaxModelWorkers: 3}); err != nil {
		t.Fatal(err)
	}
	installConfigPath(t, path)
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted, Explanation: "done"})

	code, _, stderr := runCLI(t, []string{"run", "--task", "one", "--task", "two"}, "")
	if code != 0 {
		t.Fatalf("run default = (%d, %q)", code, stderr)
	}
	for i := 1; i <= 2; i++ {
		if req := mustWorkerRequest(t, fake, i); req.Model != "acme/default" {
			t.Fatalf("worker %d model = %q, want the configured default", i, req.Model)
		}
	}
}

func TestRunThinkingUsesSavedModelWithoutPersistingEffort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Save(path, config.Config{SchemaVersion: 2, DefaultModel: "acme/default", MaxModelWorkers: 3}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	installConfigPath(t, path)
	fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted, Explanation: "done"})

	code, _, stderr := runCLI(t, []string{"run", "--thinking", "max", "--task", "work"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("run default with thinking = (%d, %q)", code, stderr)
	}
	req := mustWorkerRequest(t, fake, 1)
	if req.Model != "acme/default" || req.ThinkingLevel != pi.ThinkingMax {
		t.Fatalf("worker request = %#v", req)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("config changed: %q, %v", after, err)
	}
}

func TestRunModelExplicitEmptySelectorNeverFallsBack(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "equals form", args: []string{"run", "--model=", "--task", "work"}},
		{name: "separate argument form", args: []string{"run", "--model", "", "--task", "work"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := config.Save(path, config.Config{SchemaVersion: 2, DefaultModel: "acme/default", MaxModelWorkers: 3}); err != nil {
				t.Fatal(err)
			}
			calls := 0
			original := userConfigPath
			userConfigPath = func() (string, error) {
				calls++
				return path, nil
			}
			t.Cleanup(func() { userConfigPath = original })
			fake := installFakeWorker(t, pi.WorkerResult{Status: pi.StatusCompleted, Explanation: "done"})
			stdin := &errReader{}

			code, _, stderr := runCLIReader(t, test.args, stdin)
			if code != 2 || stderr == "" {
				t.Fatalf("run explicit empty selector = (%d, %q)", code, stderr)
			}
			if calls != 0 {
				t.Fatalf("explicit empty selector read configuration %d times", calls)
			}
			if fake.callCount() != 0 {
				t.Fatalf("explicit empty selector launched %d workers", fake.callCount())
			}
			if stdin.read {
				t.Fatal("explicit empty selector read stdin")
			}
		})
	}
}

func TestRunModelAbsentOrEmptyDefaultFailsBeforeReadingStdin(t *testing.T) {
	for _, test := range []struct {
		name  string
		write bool
	}{
		{name: "absent"},
		{name: "empty", write: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if test.write {
				if err := config.Save(path, config.Config{SchemaVersion: 2, MaxModelWorkers: 3}); err != nil {
					t.Fatal(err)
				}
			}
			installConfigPath(t, path)
			var stdout, stderr bytes.Buffer
			stdin := &errReader{}
			code := Main([]string{"run"}, stdin, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("run missing default code = %d", code)
			}
			if stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("run missing default output = (%q, %q)", stdout.String(), stderr.String())
			}
			if stdin.read {
				t.Fatal("run read stdin before rejecting the missing default model")
			}
		})
	}
}

type errReader struct{ read bool }

func (r *errReader) Read([]byte) (int, error) {
	r.read = true
	return 0, errors.New("stdin was read")
}

func TestConfigSyntaxRejectsInvalidForms(t *testing.T) {
	for _, args := range [][]string{
		{}, {"show", "--json", "extra"}, {"set"}, {"set", "default-model"}, {"set", "other", "acme/model"}, {"set", "default-model", "acme/model", "--json"}, {"set", "default-model", "acme/model", "--timeout", "0s"}, {"set", "default-model", "acme/model", "--timeout", "x"},
	} {
		if _, err := parseConfigArgs(args); err == nil {
			t.Fatalf("parseConfigArgs(%q) = nil error", args)
		}
	}
}

func TestConfigSetCatalogFailurePreservesConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	before := config.Config{SchemaVersion: 2, DefaultModel: "acme/old", MaxModelWorkers: 3}
	if err := config.Save(path, before); err != nil {
		t.Fatal(err)
	}
	installConfigPath(t, path)
	installFakeCatalog(t, &countingCatalog{err: errors.New("catalog unavailable")})
	// No --timeout: the fake catalog fails immediately, and a short deadline
	// would race that failure and turn exit 9 into exit 7.
	code, _, _ := runCLI(t, []string{"config", "set", "default-model", "acme/model"}, "")
	got, err := config.Load(path)
	if code != 9 || err != nil || got != before {
		t.Fatalf("failed catalog = code %d, config %#v, error %v", code, got, err)
	}
}

func TestConfigSetMissingExecutableExits3WithoutWritingConfig(t *testing.T) {
	// This catches treating catalog startup as an internal error or writing a
	// new default model after the read-only availability check failed.
	path := filepath.Join(t.TempDir(), "config.json")
	before := config.Config{SchemaVersion: 2, DefaultModel: "acme/old", MaxModelWorkers: 3}
	if err := config.Save(path, before); err != nil {
		t.Fatal(err)
	}
	installConfigPath(t, path)
	installFakeCatalog(t, pi.NewCatalog(filepath.Join(t.TempDir(), "missing-pi")))

	code, stdout, stderr := runCLI(t, []string{"config", "set", "default-model", "acme/model"}, "")
	if code != 3 || stdout != "" || !strings.Contains(stderr, "pi not ready") {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	got, err := config.Load(path)
	if err != nil || got != before {
		t.Fatalf("config after failed set = %#v, %v", got, err)
	}
}

// --- issue #167: max-model-workers CLI tests ---

func TestConfigSetMaxModelWorkersPreservesDefaultModelNoCatalog(t *testing.T) {
	// Setting max-model-workers on an existing schema-2 config updates only
	// that field and never touches the catalog.
	path := filepath.Join(t.TempDir(), "config.json")
	before := config.Config{SchemaVersion: 2, DefaultModel: "acme/keep", MaxModelWorkers: 3}
	if err := config.Save(path, before); err != nil {
		t.Fatal(err)
	}
	installConfigPath(t, path)
	catalog := &countingCatalog{}
	installFakeCatalog(t, catalog)

	code, stdout, stderr := runCLI(t, []string{"config", "set", "max-model-workers", "7"}, "")
	if code != 0 || stdout != "max-model-workers: 7\n" || stderr != "" {
		t.Fatalf("config set = (%d, %q, %q)", code, stdout, stderr)
	}
	if catalog.calls != 0 {
		t.Fatalf("catalog called %d times, want 0", catalog.calls)
	}
	got, err := config.Load(path)
	if err != nil || got != (config.Config{SchemaVersion: 2, DefaultModel: "acme/keep", MaxModelWorkers: 7}) {
		t.Fatalf("saved config = %#v, %v", got, err)
	}
}

func TestConfigSetMaxModelWorkersMissingConfig(t *testing.T) {
	// Setting max-model-workers when no config file exists creates a schema-2
	// document with an empty DefaultModel and the requested positive value.
	path := filepath.Join(t.TempDir(), "config.json")
	installConfigPath(t, path)
	installFakeCatalog(t, &countingCatalog{})

	code, stdout, stderr := runCLI(t, []string{"config", "set", "max-model-workers", "7"}, "")
	if code != 0 || stdout != "max-model-workers: 7\n" || stderr != "" {
		t.Fatalf("config set = (%d, %q, %q)", code, stdout, stderr)
	}
	got, err := config.Load(path)
	if err != nil || got != (config.Config{SchemaVersion: 2, MaxModelWorkers: 7}) {
		t.Fatalf("saved config = %#v, %v", got, err)
	}
}

func TestConfigSetMaxModelWorkersSchema1PreservesDefaultModel(t *testing.T) {
	// Setting max-model-workers on raw schema-1 JSON reads the defaultModel,
	// saves a schema-2 document, and preserves that value.
	path := filepath.Join(t.TempDir(), "config.json")
	raw := []byte(`{"schemaVersion":1,"defaultModel":"acme/saved"}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	installConfigPath(t, path)
	installFakeCatalog(t, &countingCatalog{})

	code, stdout, stderr := runCLI(t, []string{"config", "set", "max-model-workers", "7"}, "")
	if code != 0 || stdout != "max-model-workers: 7\n" || stderr != "" {
		t.Fatalf("config set = (%d, %q, %q)", code, stdout, stderr)
	}
	got, err := config.Load(path)
	if err != nil || got != (config.Config{SchemaVersion: 2, DefaultModel: "acme/saved", MaxModelWorkers: 7}) {
		t.Fatalf("saved config = %#v, %v", got, err)
	}
}

func TestConfigSetDefaultModelPreservesMaxModelWorkers(t *testing.T) {
	// Setting default-model on a schema-2 config with MaxModelWorkers=7
	// preserves that value after catalog validation.
	path := filepath.Join(t.TempDir(), "config.json")
	before := config.Config{SchemaVersion: 2, DefaultModel: "acme/old", MaxModelWorkers: 7}
	if err := config.Save(path, before); err != nil {
		t.Fatal(err)
	}
	installConfigPath(t, path)
	catalog := &countingCatalog{models: []pi.ModelProjection{{Provider: "acme", ID: "new"}}}
	installFakeCatalog(t, catalog)

	code, stdout, stderr := runCLI(t, []string{"config", "set", "default-model", "acme/new"}, "")
	if code != 0 || stdout != "default-model: acme/new\n" || stderr != "" {
		t.Fatalf("config set = (%d, %q, %q)", code, stdout, stderr)
	}
	got, err := config.Load(path)
	if err != nil || got != (config.Config{SchemaVersion: 2, DefaultModel: "acme/new", MaxModelWorkers: 7}) {
		t.Fatalf("saved config = %#v, %v", got, err)
	}
}

func TestConfigSetInvalidExistingConfigFailsBeforeCatalog(t *testing.T) {
	// A malformed on-disk config is rejected before the catalog is accessed,
	// and the bytes on disk are left unchanged.
	before := []byte(`{"schemaVersion":1,"unknown":true}`)
	for _, setter := range []struct {
		name string
		args []string
	}{
		{name: "default-model", args: []string{"config", "set", "default-model", "acme/model"}},
		{name: "max-model-workers", args: []string{"config", "set", "max-model-workers", "7"}},
	} {
		t.Run(setter.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, before, 0o600); err != nil {
				t.Fatal(err)
			}
			installConfigPath(t, path)
			catalog := &countingCatalog{}
			installFakeCatalog(t, catalog)

			code, stdout, stderr := runCLI(t, setter.args, "")
			if code != 9 || stdout != "" || stderr == "" || catalog.calls != 0 {
				t.Fatalf("exit = %d, stdout = %q, stderr = %q, catalog calls = %d", code, stdout, stderr, catalog.calls)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("bytes changed: %q, %v", after, err)
			}
		})
	}
}

func TestConfigShowSchema1EffectiveDocumentWithoutRewrite(t *testing.T) {
	// Raw schema-1 config show exposes the effective schema-2 document
	// without rewriting the source bytes.
	path := filepath.Join(t.TempDir(), "config.json")
	raw := []byte(`{"schemaVersion":1,"defaultModel":"acme/v1"}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	installConfigPath(t, path)

	code, stdout, stderr := runCLI(t, []string{"config", "show"}, "")
	if code != 0 || stdout != "default-model: acme/v1\nmax-model-workers: 3\n" || stderr != "" {
		t.Fatalf("config show = (%d, %q, %q)", code, stdout, stderr)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatalf("source bytes changed: %q, %v", after, err)
	}

	code, stdout, stderr = runCLI(t, []string{"config", "show", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("config show --json = (%d, %q, %q)", code, stdout, stderr)
	}
	var got config.Config
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	want := config.Config{SchemaVersion: 2, DefaultModel: "acme/v1", MaxModelWorkers: 3}
	if got != want {
		t.Fatalf("JSON decoded = %#v, want %#v", got, want)
	}
	after, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatalf("source bytes changed after JSON show: %q, %v", after, err)
	}
}

func TestConfigSetMaxModelWorkersRejectsBadSyntax(t *testing.T) {
	// All bad max-model-workers forms are rejected as usage errors before
	// the catalog is accessed and before any file write.
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "missing value", args: []string{"config", "set", "max-model-workers"}},
		{name: "zero", args: []string{"config", "set", "max-model-workers", "0"}},
		{name: "negative", args: []string{"config", "set", "max-model-workers", "-1"}},
		{name: "non-integer", args: []string{"config", "set", "max-model-workers", "abc"}},
		{name: "overflow", args: []string{"config", "set", "max-model-workers", "99999999999999999999"}},
		{name: "extra positional", args: []string{"config", "set", "max-model-workers", "7", "extra"}},
		{name: "--debug", args: []string{"config", "set", "max-model-workers", "7", "--debug"}},
		{name: "--timeout", args: []string{"config", "set", "max-model-workers", "7", "--timeout"}},
		{name: "--timeout=value", args: []string{"config", "set", "max-model-workers", "7", "--timeout=1s"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			installConfigPath(t, path)
			catalog := &countingCatalog{}
			installFakeCatalog(t, catalog)

			code, stdout, stderr := runCLI(t, tt.args, "")
			if code != 2 || stdout != "" || stderr == "" || catalog.calls != 0 {
				t.Fatalf("exit = %d, stdout = %q, stderr = %q, catalog calls = %d", code, stdout, stderr, catalog.calls)
			}
			if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("config file should not exist, err = %v", err)
			}
		})
	}
}

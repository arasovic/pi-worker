package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pi-worker/internal/config"
	"pi-worker/internal/pi"
)

type countingCatalog struct {
	models []pi.ModelProjection
	err    error
	calls  int
	req    pi.CatalogRequest
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

func runCLIReader(t *testing.T, args []string, stdin io.Reader) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Main(args, stdin, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestConfigShowHumanOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Save(path, config.Config{SchemaVersion: 1, DefaultModel: "acme/model"}); err != nil {
		t.Fatal(err)
	}
	installConfigPath(t, path)

	code, stdout, stderr := runCLI(t, []string{"config", "show"}, "")
	if code != 0 || stdout != "default-model: acme/model\n" || stderr != "" {
		t.Fatalf("config show = (%d, %q, %q)", code, stdout, stderr)
	}
}

func TestConfigShowJSONIsSingleDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Save(path, config.Config{SchemaVersion: 1}); err != nil {
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
	if got != (config.Config{SchemaVersion: 1}) || !bytes.HasSuffix([]byte(stdout), []byte("\n")) {
		t.Fatalf("JSON = %q, decoded %#v", stdout, got)
	}
}

func TestConfigShowReportsMissingAndMalformedConfig(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		installConfigPath(t, path)
		code, _, stderr := runCLI(t, []string{"config", "show"}, "")
		if code != 2 || stderr == "" {
			t.Fatalf("config show missing = (%d, %q)", code, stderr)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{"schemaVersion":1,"unexpected":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		installConfigPath(t, path)
		code, _, stderr := runCLI(t, []string{"config", "show"}, "")
		if code != 2 || stderr == "" {
			t.Fatalf("config show malformed = (%d, %q)", code, stderr)
		}
	})
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
	if err != nil || got.DefaultModel != "acme/model" {
		t.Fatalf("saved config = %#v, %v", got, err)
	}
}

func TestConfigSetRejectsInvalidSyntaxBeforeCatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	installConfigPath(t, path)
	catalog := &countingCatalog{}
	installFakeCatalog(t, catalog)

	code, _, stderr := runCLI(t, []string{"config", "set", "default-model", "acme/model:thinking"}, "")
	if code != 2 || stderr == "" || catalog.calls != 0 {
		t.Fatalf("config set invalid = (%d, %q), calls=%d", code, stderr, catalog.calls)
	}
}

func TestConfigSetUnavailableDoesNotChangeConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	before := config.Config{SchemaVersion: 1, DefaultModel: "acme/old"}
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
	if err := config.Save(path, config.Config{SchemaVersion: 1, DefaultModel: "acme/default"}); err != nil {
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
			if err := config.Save(path, config.Config{SchemaVersion: 1, DefaultModel: "acme/default"}); err != nil {
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
				if err := config.Save(path, config.Config{SchemaVersion: 1}); err != nil {
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
	before := config.Config{SchemaVersion: 1, DefaultModel: "acme/old"}
	if err := config.Save(path, before); err != nil {
		t.Fatal(err)
	}
	installConfigPath(t, path)
	installFakeCatalog(t, &countingCatalog{err: errors.New("catalog unavailable")})
	code, _, _ := runCLI(t, []string{"config", "set", "default-model", "acme/model", "--timeout", (10 * time.Millisecond).String()}, "")
	got, err := config.Load(path)
	if code != 9 || err != nil || got != before {
		t.Fatalf("failed catalog = code %d, config %#v, error %v", code, got, err)
	}
}

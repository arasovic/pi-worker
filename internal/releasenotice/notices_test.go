package releasenotice

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestInventoryMatchesFixedModuleSet(t *testing.T) {
	t.Helper()

	got := Inventory()
	want := []Dependency{
		{Module: "github.com/shirou/gopsutil/v4", Version: "v4.26.7", Targets: []string{"darwin", "linux"}, LicenseFiles: []string{"LICENSE"}},
		{Module: "golang.org/x/sys", Version: "v0.47.0", Targets: []string{"darwin", "linux"}, LicenseFiles: []string{"LICENSE", "PATENTS"}},
		{Module: "github.com/tklauser/go-sysconf", Version: "v0.3.16", Targets: []string{"darwin", "linux"}, LicenseFiles: []string{"LICENSE"}},
		{Module: "github.com/ebitengine/purego", Version: "v0.10.2", Targets: []string{"darwin"}, LicenseFiles: []string{"LICENSE"}},
		{Module: "github.com/tklauser/numcpus", Version: "v0.11.0", Targets: []string{"linux"}, LicenseFiles: []string{"LICENSE"}},
		{Module: "golang.org/x/term", Version: "v0.45.0", Targets: []string{"darwin", "linux"}, LicenseFiles: []string{"LICENSE", "PATENTS"}},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Inventory() mismatch\n got: %#v\nwant: %#v", got, want)
	}

	copy := Inventory()
	if len(copy) != len(got) {
		t.Fatalf("Inventory() copy length mismatch")
	}
	copy[0].Targets[0] = "mutated"
	if got[0].Targets[0] == copy[0].Targets[0] {
		t.Fatalf("Inventory() returns mutable shared dependency list")
	}
}

func TestRenderWritesDeterministicNoticeContent(t *testing.T) {
	t.Helper()

	moduleCache := t.TempDir()
	fixtures := map[string]map[string]string{
		"github.com/shirou/gopsutil/v4@v4.26.7": {
			"LICENSE": "gopsutil license line A\nline B\n\n",
		},
		"golang.org/x/sys@v0.47.0": {
			"LICENSE": "sys license\n\n",
			"PATENTS": "sys patents\n",
		},
		"github.com/tklauser/go-sysconf@v0.3.16": {
			"LICENSE": "sysconf license\n\n",
		},
		"github.com/ebitengine/purego@v0.10.2": {
			"LICENSE": "purego license\n\n",
		},
		"github.com/tklauser/numcpus@v0.11.0": {
			"LICENSE": "numcpus license\n",
		},
		"golang.org/x/term@v0.45.0": {
			"LICENSE": "term license\n",
			"PATENTS": "term patents\n",
		},
	}
	for moduleVersion, files := range fixtures {
		for file, content := range files {
			path := filepath.Join(moduleCache, filepath.FromSlash(moduleVersion), file)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("mkdir fixture module dir: %v", err)
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatalf("write fixture module file: %v", err)
			}
		}
	}

	raw, err := Render(moduleCache)
	if err != nil {
		t.Fatalf("Render() unexpected error: %v", err)
	}
	content := string(raw)

	if !strings.HasSuffix(content, "\n") {
		t.Fatalf("rendered notices missing terminal newline")
	}
	if strings.HasSuffix(content, "\n\n") {
		t.Fatalf("rendered notices contain a blank line at EOF")
	}

	order := []string{
		"## github.com/shirou/gopsutil/v4 v4.26.7",
		"## golang.org/x/sys v0.47.0",
		"## github.com/tklauser/go-sysconf v0.3.16",
		"## github.com/ebitengine/purego v0.10.2",
		"## github.com/tklauser/numcpus v0.11.0",
		"## golang.org/x/term v0.45.0",
	}
	last := -1
	for _, header := range order {
		idx := strings.Index(content, header)
		if idx < 0 {
			t.Fatalf("expected module section %q in rendered notices", header)
		}
		if idx <= last {
			t.Fatalf("module section order not stable for %q", header)
		}
		if strings.Count(content, header) != 1 {
			t.Fatalf("module section duplicated: %q", header)
		}
		dep := dependencyForHeader(t, header)
		if !strings.Contains(content, "## "+dep.Module+" "+dep.Version+"\nTargets: "+strings.Join(dep.Targets, ", ")) {
			t.Fatalf("targets block missing for %q", header)
		}
		last = idx
	}

	for moduleVersion, files := range fixtures {
		for file, expectedText := range files {
			if !strings.Contains(content, expectedText) {
				t.Fatalf("missing fixture text for %q %q", moduleVersion, file)
			}
		}
	}

	xsysSection := sectionForModule(content, "golang.org/x/sys v0.47.0")
	if !strings.Contains(xsysSection, "### LICENSE") || !strings.Contains(xsysSection, "### PATENTS") {
		t.Fatalf("expected x/sys to contain separate LICENSE and PATENTS sections")
	}
	if !strings.Contains(xsysSection, fixtures["golang.org/x/sys@v0.47.0"]["LICENSE"]) {
		t.Fatalf("x/sys LICENSE content missing")
	}
	if !strings.Contains(xsysSection, fixtures["golang.org/x/sys@v0.47.0"]["PATENTS"]) {
		t.Fatalf("x/sys PATENTS content missing")
	}
}

func TestRenderStartsWithGeneratedPreamble(t *testing.T) {
	t.Helper()

	moduleCache := t.TempDir()
	writeNoticeFixtureFiles(t, moduleCache)

	raw, err := Render(moduleCache)
	if err != nil {
		t.Fatalf("Render() unexpected error: %v", err)
	}
	content := string(raw)

	firstModule := "## github.com/shirou/gopsutil/v4 v4.26.7"
	if !strings.HasPrefix(content, preamble) {
		t.Fatalf("rendered notices do not start with the generated preamble")
	}
	if !strings.HasPrefix(content, "# Third-Party Notices\n") {
		t.Fatalf("rendered notices do not start with the top-level title")
	}
	if !strings.Contains(content, "`[yyyy] [name of copyright owner]`") {
		t.Fatalf("preamble missing Apache placeholder explanation")
	}
	if !strings.Contains(content, "go run ./tools/notices --write THIRD_PARTY_NOTICES") {
		t.Fatalf("preamble missing generation instruction")
	}
	if !strings.HasPrefix(content[len(preamble):], firstModule) {
		t.Fatalf("first module header must immediately follow the preamble")
	}
}

func TestRenderRejectsMissingFixtureFiles(t *testing.T) {
	t.Helper()

	t.Run("missing license", func(t *testing.T) {
		t.Helper()
		moduleCache := t.TempDir()
		module := "github.com/shirou/gopsutil/v4@v4.26.7"
		dir := filepath.Join(moduleCache, filepath.FromSlash(module))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir module dir: %v", err)
		}
		if _, err := Render(moduleCache); err == nil {
			t.Fatalf("expected render to fail with missing license file")
		}
	})

	t.Run("missing patents", func(t *testing.T) {
		moduleCache := t.TempDir()
		writeNoticeFixtureFiles(t, moduleCache)
		if err := os.Remove(filepath.Join(moduleCache, filepath.FromSlash("golang.org/x/sys@v0.47.0"), "PATENTS")); err != nil {
			t.Fatalf("remove fixture patents file: %v", err)
		}
		if _, err := Render(moduleCache); err == nil {
			t.Fatalf("expected render to fail with missing PATENTS file")
		}
	})
}

func TestVerifyMatchesRenderedNotice(t *testing.T) {
	t.Helper()

	moduleCache := t.TempDir()
	writeNoticeFixtureFiles(t, moduleCache)

	raw, err := Render(moduleCache)
	if err != nil {
		t.Fatalf("Render() unexpected error: %v", err)
	}

	if err := Verify(raw, moduleCache); err != nil {
		t.Fatalf("Verify() mismatch on matching notices: %v", err)
	}

	broken := append([]byte{}, raw...)
	broken = append(broken, "mismatch"...)
	if err := Verify(broken, moduleCache); err == nil {
		t.Fatalf("Verify() should fail when notice content differs")
	}
}

func TestInventoryMatchesTargetDependencyUnion(t *testing.T) {
	targets := []struct {
		goos   string
		goarch string
	}{
		{goos: "darwin", goarch: "amd64"},
		{goos: "darwin", goarch: "arm64"},
		{goos: "linux", goarch: "amd64"},
		{goos: "linux", goarch: "arm64"},
	}

	got := make(map[string]string)
	for _, target := range targets {
		for module, version := range modulesForTarget(t, target.goos, target.goarch) {
			got[module] = version
		}
	}

	want := make(map[string]string)
	for _, dep := range Inventory() {
		want[dep.Module] = dep.Version
	}

	if len(got) != len(want) {
		t.Fatalf("dependency graph size mismatch; got=%d want=%d", len(got), len(want))
	}
	for module, version := range want {
		gotVersion, ok := got[module]
		if !ok {
			t.Fatalf("missing dependency %q in target graph union", module)
		}
		if gotVersion != version {
			t.Fatalf("version mismatch for %q: got %q want %q", module, gotVersion, version)
		}
	}
	for module := range got {
		if _, ok := want[module]; !ok {
			t.Fatalf("unexpected dependency present in union: %q", module)
		}
	}
}

func writeNoticeFixtureFiles(t *testing.T, moduleCache string) {
	t.Helper()
	fixtures := map[string]map[string]string{
		"github.com/shirou/gopsutil/v4@v4.26.7": {
			"LICENSE": "gopsutil license line A\nline B\n\n",
		},
		"golang.org/x/sys@v0.47.0": {
			"LICENSE": "sys license\n\n",
			"PATENTS": "sys patents\n",
		},
		"github.com/tklauser/go-sysconf@v0.3.16": {
			"LICENSE": "sysconf license\n\n",
		},
		"github.com/ebitengine/purego@v0.10.2": {
			"LICENSE": "purego license\n\n",
		},
		"github.com/tklauser/numcpus@v0.11.0": {
			"LICENSE": "numcpus license\n",
		},
		"golang.org/x/term@v0.45.0": {
			"LICENSE": "term license\n",
			"PATENTS": "term patents\n",
		},
	}
	for moduleVersion, files := range fixtures {
		for file, content := range files {
			path := filepath.Join(moduleCache, filepath.FromSlash(moduleVersion), file)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("mkdir fixture module dir: %v", err)
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatalf("write fixture module file: %v", err)
			}
		}
	}
}

func modulesForTarget(t *testing.T, goos, goarch string) map[string]string {
	t.Helper()

	cmd := exec.Command("go", "list", "-deps", "-json", "./cmd/pi-worker")
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	cmd.Dir = filepath.Join("..", "..")
	// Keep stderr off stdout: a cold module cache makes `go list` report
	// "go: downloading ..." progress, which would corrupt the JSON stream below.
	stdout, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		stderr := ""
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		t.Fatalf("go list failed for %s/%s: %v\n%s", goos, goarch, err, stderr)
	}

	decoder := json.NewDecoder(bytes.NewReader(stdout))
	type moduleDesc struct {
		Path    string `json:"Path"`
		Version string `json:"Version"`
		Main    bool   `json:"Main"`
	}
	type packageJSON struct {
		Module *moduleDesc `json:"Module"`
	}

	deps := make(map[string]string)
	for {
		var data packageJSON
		err := decoder.Decode(&data)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("decode go list JSON: %v", err)
		}
		if data.Module == nil || data.Module.Main || data.Module.Path == "" {
			continue
		}
		deps[data.Module.Path] = data.Module.Version
	}

	return deps
}

func sectionForModule(content, header string) string {
	start := strings.Index(content, "## "+header+"\n")
	if start == -1 {
		return ""
	}
	end := len(content)
	if next := strings.Index(content[start+1:], "\n## "); next != -1 {
		end = start + 1 + next
	}
	return content[start:end]
}

func dependencyForHeader(t *testing.T, header string) Dependency {
	t.Helper()
	for _, dep := range Inventory() {
		candidate := "## " + dep.Module + " " + dep.Version
		if candidate == header {
			return dep
		}
	}
	t.Fatalf("header %q not found in inventory", header)
	return Dependency{}
}

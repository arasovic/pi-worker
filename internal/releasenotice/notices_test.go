package releasenotice

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestInventoryDeclaresDecisionsOnly states what the declared inventory is for:
// which modules are distributed, on which targets, with which notice files. It
// names no version, because a version is not a decision this package makes.
func TestInventoryDeclaresDecisionsOnly(t *testing.T) {
	t.Helper()

	inventory := Inventory()
	if len(inventory) == 0 {
		t.Fatalf("Inventory() declares no dependency")
	}

	platforms := map[string]bool{"darwin": true, "linux": true}
	for _, dep := range inventory {
		if dep.Module == "" {
			t.Fatalf("Inventory() declares a dependency without a module")
		}
		if len(dep.Targets) == 0 {
			t.Fatalf("dependency %q declares no target", dep.Module)
		}
		for _, target := range dep.Targets {
			if !platforms[target] {
				t.Fatalf("dependency %q declares target %q, which is not a release platform", dep.Module, target)
			}
		}
		if len(dep.LicenseFiles) == 0 {
			t.Fatalf("dependency %q declares no notice file", dep.Module)
		}
		for _, file := range dep.LicenseFiles {
			if filepath.Base(file) != file {
				t.Fatalf("dependency %q declares notice path %q, which must name a file inside the module", dep.Module, file)
			}
		}
	}

	clone := Inventory()
	if !reflect.DeepEqual(clone, inventory) {
		t.Fatalf("Inventory() is not stable across calls:\n got: %#v\nwant: %#v", clone, inventory)
	}
	clone[0].Targets[0] = "mutated"
	clone[0].LicenseFiles[0] = "MUTATED"
	if inventory[0].Targets[0] == "mutated" || inventory[0].LicenseFiles[0] == "MUTATED" {
		t.Fatalf("Inventory() returns mutable shared dependency slices")
	}
}

func TestRenderWritesDeterministicNoticeContent(t *testing.T) {
	t.Helper()

	inventory := Inventory()
	moduleCache := t.TempDir()
	fixtures := writeNoticeFixtures(t, moduleCache)
	versions := resolvedVersions(t)

	raw, err := Render(moduleCache)
	if err != nil {
		t.Fatalf("Render() unexpected error: %v", err)
	}
	second, err := Render(moduleCache)
	if err != nil {
		t.Fatalf("second Render() unexpected error: %v", err)
	}
	if !bytes.Equal(raw, second) {
		t.Fatalf("Render() is not deterministic")
	}
	content := string(raw)

	if !strings.HasSuffix(content, "\n") {
		t.Fatalf("rendered notices missing terminal newline")
	}
	if strings.HasSuffix(content, "\n\n") {
		t.Fatalf("rendered notices contain a blank line at EOF")
	}
	lastDep := inventory[len(inventory)-1]
	lastNotice := lastDep.LicenseFiles[len(lastDep.LicenseFiles)-1]
	if want := fixtures[noticeKey(lastDep, versions)][lastNotice]; !strings.HasSuffix(content, want) {
		t.Fatalf("rendered notices do not end with the last notice block verbatim; want suffix %q", want)
	}

	// Every declared module gets exactly one heading, in declaration order,
	// naming the version the build selects. The notice bytes under a heading have
	// to be the bytes of that same version: each fixture repeats the module@version
	// directory it lives in, so a heading and a body that disagree fail here
	// instead of agreeing on a version nothing builds with.
	last := -1
	for _, dep := range inventory {
		version := versions[dep.Module]
		header := "## " + dep.Module + " " + version
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
		if !strings.Contains(content, header+"\nTargets: "+strings.Join(dep.Targets, ", ")) {
			t.Fatalf("targets block missing for %q", header)
		}
		last = idx

		section := sectionForModule(content, dep.Module+" "+version)
		if section == "" {
			t.Fatalf("module section %q missing from rendered notices", dep.Module)
		}
		for _, file := range dep.LicenseFiles {
			if !strings.Contains(section, "### "+file) {
				t.Fatalf("notice block for %q %s missing", dep.Module, file)
			}
			if want := fixtures[noticeKey(dep, versions)][file]; !strings.Contains(section, want) {
				t.Fatalf("section for %q does not contain the %s of %s: %q",
					dep.Module, file, noticeKey(dep, versions), want)
			}
		}
	}

	// A heading is always a module and a version, never a bare module: an empty
	// version cannot reach the document.
	for _, line := range strings.Split(content, "\n") {
		if !strings.HasPrefix(line, "## ") {
			continue
		}
		if fields := strings.Fields(line); len(fields) != 3 || !strings.HasPrefix(fields[2], "v") {
			t.Fatalf("module heading %q does not name a module and one build-selected version", line)
		}
	}

	multi := dependencyWithSeveralNoticeFiles(t)
	section := sectionForModule(content, multi.Module+" "+versions[multi.Module])
	for _, file := range multi.LicenseFiles {
		if !strings.Contains(section, "### "+file) {
			t.Fatalf("expected %s to render %s as its own section", multi.Module, file)
		}
	}
}

func TestRenderStartsWithGeneratedPreamble(t *testing.T) {
	t.Helper()

	inventory := Inventory()
	moduleCache := t.TempDir()
	writeNoticeFixtures(t, moduleCache)
	versions := resolvedVersions(t)

	raw, err := Render(moduleCache)
	if err != nil {
		t.Fatalf("Render() unexpected error: %v", err)
	}
	content := string(raw)

	firstModule := "## " + inventory[0].Module + " " + versions[inventory[0].Module]
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

func TestRenderRejectsMissingNoticeFiles(t *testing.T) {
	t.Helper()

	inventory := Inventory()
	versions := resolvedVersions(t)
	multi := dependencyWithSeveralNoticeFiles(t)

	t.Run("missing license", func(t *testing.T) {
		t.Helper()
		moduleCache := t.TempDir()
		writeNoticeFixtures(t, moduleCache)

		dep := inventory[0]
		path := filepath.Join(moduleCache, filepath.FromSlash(noticeKey(dep, versions)), dep.LicenseFiles[0])
		if err := os.Remove(path); err != nil {
			t.Fatalf("remove fixture notice file: %v", err)
		}
		_, err := Render(moduleCache)
		if err == nil {
			t.Fatalf("expected render to fail with a missing license file")
		}
		if !strings.Contains(err.Error(), dep.Module) || !strings.Contains(err.Error(), versions[dep.Module]) {
			t.Fatalf("read failure must name the module and its selected version; got: %v", err)
		}
	})

	t.Run("missing second notice file", func(t *testing.T) {
		t.Helper()
		moduleCache := t.TempDir()
		writeNoticeFixtures(t, moduleCache)

		file := multi.LicenseFiles[1]
		path := filepath.Join(moduleCache, filepath.FromSlash(noticeKey(multi, versions)), file)
		if err := os.Remove(path); err != nil {
			t.Fatalf("remove fixture notice file: %v", err)
		}
		if _, err := Render(moduleCache); err == nil {
			t.Fatalf("expected render to fail with a missing %s file", file)
		}
	})

	t.Run("empty module cache", func(t *testing.T) {
		t.Helper()
		if _, err := Render(t.TempDir()); err == nil {
			t.Fatalf("expected render to fail with an empty module cache")
		}
	})
}

func TestVerifyMatchesRenderedNotice(t *testing.T) {
	t.Helper()

	moduleCache := t.TempDir()
	writeNoticeFixtures(t, moduleCache)

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

// TestInventoryMatchesTargetDependencyUnion checks the decisions the package
// declares against what the release targets actually build: the declared module
// set is the union of the distributed dependencies, and each module's declared
// targets are the platforms whose build list carries it. No version literal
// participates, so a dependency bump cannot turn this red.
func TestInventoryMatchesTargetDependencyUnion(t *testing.T) {
	t.Helper()

	targets := []struct {
		goos   string
		goarch string
	}{
		{goos: "darwin", goarch: "amd64"},
		{goos: "darwin", goarch: "arm64"},
		{goos: "linux", goarch: "amd64"},
		{goos: "linux", goarch: "arm64"},
	}

	seenOn := make(map[string]map[string]bool)
	graphVersion := make(map[string]string)
	for _, target := range targets {
		for module, version := range modulesForTarget(t, target.goos, target.goarch) {
			if seenOn[module] == nil {
				seenOn[module] = make(map[string]bool)
			}
			seenOn[module][target.goos] = true
			if prior, ok := graphVersion[module]; ok && prior != version {
				t.Fatalf("release targets disagree on the version of %q: %q and %q", module, prior, version)
			}
			graphVersion[module] = version
		}
	}

	inventory := Inventory()
	declared := make(map[string]bool, len(inventory))
	for _, dep := range inventory {
		if declared[dep.Module] {
			t.Fatalf("dependency %q declared twice", dep.Module)
		}
		declared[dep.Module] = true
	}

	for module := range seenOn {
		if _, ok := declared[module]; !ok {
			t.Fatalf("unexpected dependency present in union: %q", module)
		}
	}
	for _, dep := range inventory {
		targets, ok := seenOn[dep.Module]
		if !ok {
			t.Fatalf("missing dependency %q in target graph union", dep.Module)
		}
		// Compare the graph's answer directly, so a target that pulled in no
		// dependency at all cannot pass the set comparison by accident.
		graph := sortedTargetSet(targets)
		if want := sortedTargetSet(stringSet(dep.Targets)); !reflect.DeepEqual(graph, want) {
			t.Fatalf("target membership mismatch for %q: graph=%v declared=%v", dep.Module, graph, want)
		}
	}
}

// TestRenderNamesTheVersionTheBuildSelects ties the rendered document to the
// build lists the release targets actually compile: the heading version of every
// distributed module is what the go command selects and what each of the
// module's own targets carries, so the document cannot name a version no binary
// contains.
func TestRenderNamesTheVersionTheBuildSelects(t *testing.T) {
	t.Helper()

	buildList := map[string]map[string]string{
		"darwin": modulesForTarget(t, "darwin", "arm64"),
		"linux":  modulesForTarget(t, "linux", "amd64"),
	}
	versions, err := SelectedVersions()
	if err != nil {
		t.Fatalf("SelectedVersions() unexpected error: %v", err)
	}

	moduleCache := t.TempDir()
	writeNoticeFixtures(t, moduleCache)
	raw, err := Render(moduleCache)
	if err != nil {
		t.Fatalf("Render() unexpected error: %v", err)
	}
	content := string(raw)

	for _, dep := range Inventory() {
		selected := versions[dep.Module]
		if strings.Count(content, "## "+dep.Module+" "+selected+"\n") != 1 {
			t.Fatalf("rendered notices do not name the build-selected version %q of %q", selected, dep.Module)
		}
		for _, goos := range dep.Targets {
			carried, ok := buildList[goos][dep.Module]
			if !ok {
				t.Fatalf("module %q: the %s release target builds no version of it", dep.Module, goos)
			}
			if carried != selected {
				t.Fatalf("module %q: the notices name %q, but the %s release target compiles %q",
					dep.Module, selected, goos, carried)
			}
		}
	}
}

// noticeKey is the module cache directory holding dep's selected version.
func noticeKey(dep Dependency, versions map[string]string) string {
	return dep.Module + "@" + versions[dep.Module]
}

// dependencyWithSeveralNoticeFiles returns a declared dependency whose notices
// are more than one file, the case the renderer has to keep separate.
func dependencyWithSeveralNoticeFiles(t *testing.T) Dependency {
	t.Helper()
	for _, dep := range Inventory() {
		if len(dep.LicenseFiles) > 1 {
			return dep
		}
	}
	t.Fatalf("the inventory declares no dependency with more than one notice file")
	return Dependency{}
}

// resolvedVersions resolves the declared inventory against the build, the same
// way Render does.
func resolvedVersions(t *testing.T) map[string]string {
	t.Helper()
	versions, err := SelectedVersions()
	if err != nil {
		t.Fatalf("SelectedVersions() unexpected error: %v", err)
	}
	for _, dep := range Inventory() {
		if _, ok := versions[dep.Module]; !ok {
			t.Fatalf("the build selected no version for declared module %q", dep.Module)
		}
	}
	return versions
}

// writeNoticeFixtures builds a module cache holding the declared notice files at
// the versions the build selects. Each fixture repeats the module@version
// directory it lives in, so a document naming a version other than the bytes it
// carries is caught rather than averaged away.
func writeNoticeFixtures(t *testing.T, moduleCache string) map[string]map[string]string {
	t.Helper()

	versions := resolvedVersions(t)
	fixtures := make(map[string]map[string]string)
	for _, dep := range Inventory() {
		key := noticeKey(dep, versions)
		fixtures[key] = make(map[string]string, len(dep.LicenseFiles))
		for _, file := range dep.LicenseFiles {
			content := fmt.Sprintf("%s notice text for %s\n", strings.ToLower(file), key)
			path := filepath.Join(moduleCache, filepath.FromSlash(key), file)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("mkdir fixture module dir: %v", err)
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatalf("write fixture module file: %v", err)
			}
			fixtures[key][file] = content
		}
	}
	return fixtures
}

// modulesForTarget returns the module versions the build list of one release
// target carries, read from the go command for that GOOS/GOARCH pair.
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

func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func sortedTargetSet(targets map[string]bool) []string {
	result := make([]string, 0, len(targets))
	for target := range targets {
		result = append(result, target)
	}
	sort.Strings(result)
	return result
}

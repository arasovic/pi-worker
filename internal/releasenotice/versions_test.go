package releasenotice

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// unselectedModule is a module that no build of this repository selects. It
// forces the resolver's unresolved path.
const unselectedModule = "example.com/pi-worker/definitely-not-a-dependency"

func TestSelectedVersionsResolvesEveryDeclaredModule(t *testing.T) {
	t.Helper()

	inventory := Inventory()
	got, err := SelectedVersions()
	if err != nil {
		t.Fatalf("SelectedVersions() unexpected error: %v", err)
	}

	if len(got) != len(inventory) {
		t.Fatalf("SelectedVersions() resolved %d modules, want the %d declared ones: %v",
			len(got), len(inventory), sortedKeys(got))
	}
	for _, dep := range inventory {
		version, ok := got[dep.Module]
		if !ok {
			t.Fatalf("SelectedVersions() did not resolve declared module %q", dep.Module)
		}
		if version == "" {
			t.Fatalf("SelectedVersions() resolved module %q to an empty version", dep.Module)
		}
		if !strings.HasPrefix(version, "v") {
			t.Fatalf("SelectedVersions() resolved module %q to %q, which is not a module version", dep.Module, version)
		}
	}
}

// TestSelectedVersionsFailsForAModuleTheBuildDoesNotSelect is the guard: a
// version that cannot be resolved has to fail, naming the module, rather than
// resolve to nothing and render a blank version into the notice document.
func TestSelectedVersionsFailsForAModuleTheBuildDoesNotSelect(t *testing.T) {
	t.Helper()

	_, err := selectedVersions(t.Context(), []string{unselectedModule})
	if err == nil {
		t.Fatalf("selectedVersions() succeeded for %q, which no build of this module selects", unselectedModule)
	}
	if !strings.Contains(err.Error(), unselectedModule) {
		t.Fatalf("selectedVersions() error must name the unresolved module; got: %v", err)
	}
	if !errors.Is(err, errNoSelectedVersion) {
		t.Fatalf("selectedVersions() error must be %v; got: %v", errNoSelectedVersion, err)
	}
}

// TestRenderFailsWhenTheBuildSelectsNoVersionForADependency guards the same
// property from the rendering side: Render cannot name versions taken from the
// build, so it refuses to produce a document at all. Each stand-in below is an
// answer a go command really gives when the build selects nothing for a declared
// module: a stripped or failing toolchain, an empty build list, a report without
// a version, a report about some other module.
func TestRenderFailsWhenTheBuildSelectsNoVersionForADependency(t *testing.T) {
	t.Helper()

	inventory := Inventory()
	answers := func(format string) func(context.Context, string, ...string) (goRun, error) {
		return func(context.Context, string, ...string) (goRun, error) {
			var stdout strings.Builder
			for _, dep := range inventory {
				fmt.Fprintf(&stdout, format, dep.Module)
			}
			return goRun{stdout: stdout.String()}, nil
		}
	}

	original := goList
	t.Cleanup(func() { goList = original })

	for _, standIn := range []struct {
		name string
		run  func(context.Context, string, ...string) (goRun, error)
	}{
		{name: "the go command fails", run: func(context.Context, string, ...string) (goRun, error) {
			return goRun{stderr: "go: " + unselectedModule + ": not a known dependency"}, errors.New("exit status 1")
		}},
		{name: "the go command answers nothing", run: func(context.Context, string, ...string) (goRun, error) {
			return goRun{}, nil
		}},
		{name: "the go command answers an empty version", run: answers("%s \n")},
		{name: "the go command answers no version column", run: answers("%s\n")},
		{name: "the go command answers only an unrelated module", run: func(context.Context, string, ...string) (goRun, error) {
			return goRun{stdout: unselectedModule + " v9.9.9\n"}, nil
		}},
	} {
		t.Run(standIn.name, func(t *testing.T) {
			t.Helper()
			goList = standIn.run
			_, err := Render(t.TempDir())
			if err == nil {
				t.Fatalf("Render() produced a document without a version selected by the build")
			}
			if !errors.Is(err, errNoSelectedVersion) {
				t.Fatalf("Render() error must report the missing version; got: %v", err)
			}
			if !strings.Contains(err.Error(), inventory[0].Module) {
				t.Fatalf("Render() error must name an unresolved module; got: %v", err)
			}
		})
	}
}

// TestSelectedVersionRejectsAnUnresolvedModule covers the last check between the
// build list and a rendered heading: a module with no resolved version fails by
// name instead of rendering a heading with a blank version.
func TestSelectedVersionRejectsAnUnresolvedModule(t *testing.T) {
	t.Helper()

	module := Inventory()[0].Module
	resolved := "resolved-by-the-build"

	for name, versions := range map[string]map[string]string{
		"missing from the resolution":  {},
		"resolved to the empty string": {module: ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Helper()
			version, err := selectedVersion(versions, module)
			if err == nil {
				t.Fatalf("selectedVersion() returned %q for %q without a version from the build", version, module)
			}
			if !strings.Contains(err.Error(), module) {
				t.Fatalf("selectedVersion() error must name the module; got: %v", err)
			}
			if !errors.Is(err, errNoSelectedVersion) {
				t.Fatalf("selectedVersion() error must be %v; got: %v", errNoSelectedVersion, err)
			}
		})
	}

	version, err := selectedVersion(map[string]string{module: resolved}, module)
	if err != nil || version != resolved {
		t.Fatalf("selectedVersion() resolved %q to %q, err %v", module, version, err)
	}
}

// TestGoComplaintReportsTheRealFailure keeps the resolver's error useful on a
// cold module cache, where go reports download progress before it explains
// itself.
func TestGoComplaintReportsTheRealFailure(t *testing.T) {
	t.Helper()

	complaint := "go: module " + unselectedModule + ": not a known dependency"
	cold := "go: downloading " + Inventory()[0].Module + " v9.9.9\n" + complaint + "\n"
	if got := goComplaint(cold); got != complaint {
		t.Fatalf("goComplaint() reported %q, want %q", got, complaint)
	}
	if got := goComplaint("go: downloading example.com/module v9.9.9\n"); !strings.Contains(got, "without an explanation") {
		t.Fatalf("goComplaint() reported %q for a stderr holding no complaint", got)
	}
}

// TestModuleRootFindsTheGoverningGoMod pins the directory the resolver asks: the
// module whose build the notices describe, not whatever directory a caller
// happens to be standing in.
func TestModuleRootFindsTheGoverningGoMod(t *testing.T) {
	t.Helper()

	root, err := moduleRoot()
	if err != nil {
		t.Fatalf("moduleRoot() unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("moduleRoot() reported %q, which holds no go.mod: %v", root, err)
	}
	versions, err := selectedVersions(context.Background(), []string{Inventory()[0].Module})
	if err != nil {
		t.Fatalf("selectedVersions() from %q unexpected error: %v", root, err)
	}
	if versions[Inventory()[0].Module] == "" {
		t.Fatalf("selectedVersions() resolved no version from %q", root)
	}
}

// sortedKeys reports the modules a resolution answered, for failure text.
func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

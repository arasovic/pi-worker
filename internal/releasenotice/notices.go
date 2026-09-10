package releasenotice

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Dependency describes a third-party module and which notice files to include.
// Which version of the module the artifact carries is not declared here: it is
// the version the build selects, reported by SelectedVersions.
type Dependency struct {
	Module       string
	Targets      []string
	LicenseFiles []string
}

var fixedInventory = []Dependency{
	{Module: "github.com/shirou/gopsutil/v4", Targets: []string{"darwin", "linux"}, LicenseFiles: []string{"LICENSE"}},
	{Module: "golang.org/x/sys", Targets: []string{"darwin", "linux"}, LicenseFiles: []string{"LICENSE", "PATENTS"}},
	{Module: "github.com/tklauser/go-sysconf", Targets: []string{"darwin", "linux"}, LicenseFiles: []string{"LICENSE"}},
	{Module: "github.com/ebitengine/purego", Targets: []string{"darwin"}, LicenseFiles: []string{"LICENSE"}},
	{Module: "github.com/tklauser/numcpus", Targets: []string{"linux"}, LicenseFiles: []string{"LICENSE"}},
	{Module: "golang.org/x/term", Targets: []string{"darwin", "linux"}, LicenseFiles: []string{"LICENSE", "PATENTS"}},
}

// Inventory returns the exact dependency set included in release artifacts.
func Inventory() []Dependency {
	inv := make([]Dependency, len(fixedInventory))
	for i, dep := range fixedInventory {
		clone := dep
		clone.Targets = append([]string(nil), dep.Targets...)
		clone.LicenseFiles = append([]string(nil), dep.LicenseFiles...)
		inv[i] = clone
	}
	return inv
}

const preamble = "# Third-Party Notices\n\n" +
	"Pi Worker release archives bundle the modules listed below. Every `###` block\n" +
	"reproduces an upstream license file verbatim. Placeholder text inside those\n" +
	"blocks, such as the `[yyyy] [name of copyright owner]` line in an Apache-2.0\n" +
	"appendix, is part of the upstream file and is intentionally left unchanged.\n\n" +
	"This file is generated. Run `go run ./tools/notices --write THIRD_PARTY_NOTICES`\n" +
	"instead of editing it by hand.\n\n"

// Render renders the third-party notice document from a module cache path. The
// version named for each module is the version the build selects.
func Render(moduleCache string) ([]byte, error) {
	var b strings.Builder
	b.WriteString(preamble)
	inventory := Inventory()
	versions, err := SelectedVersions()
	if err != nil {
		return nil, err
	}
	for i, dep := range inventory {
		version, err := selectedVersion(versions, dep.Module)
		if err != nil {
			return nil, err
		}
		b.WriteString("## ")
		b.WriteString(dep.Module)
		b.WriteByte(' ')
		b.WriteString(version)
		b.WriteByte('\n')
		b.WriteString("Targets: ")
		b.WriteString(strings.Join(dep.Targets, ", "))
		b.WriteByte('\n')
		b.WriteByte('\n')

		for j, fileName := range dep.LicenseFiles {
			path := filepath.Join(moduleCache, filepath.FromSlash(dep.Module+"@"+version), fileName)
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read %s %s %s: %w", dep.Module, version, fileName, err)
			}

			b.WriteString("### ")
			b.WriteString(fileName)
			b.WriteByte('\n')
			b.WriteByte('\n')
			b.Write(data)
			if len(data) == 0 || data[len(data)-1] != '\n' {
				b.WriteByte('\n')
			}
			if i+1 < len(inventory) || j+1 < len(dep.LicenseFiles) {
				b.WriteByte('\n')
			}
		}
	}

	result := b.String()
	if !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	return []byte(result), nil
}

// Verify verifies that content is exactly what Render would produce.
func Verify(content []byte, moduleCache string) error {
	expected, err := Render(moduleCache)
	if err != nil {
		return err
	}
	if !bytes.Equal(content, expected) {
		return fmt.Errorf("third party notices differ from rendered output")
	}
	return nil
}

package releasenotice

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Dependency describes a third-party module and which notice files to include.
type Dependency struct {
	Module       string
	Version      string
	Targets      []string
	LicenseFiles []string
}

var fixedInventory = []Dependency{
	{Module: "github.com/shirou/gopsutil/v4", Version: "v4.26.6", Targets: []string{"darwin", "linux"}, LicenseFiles: []string{"LICENSE"}},
	{Module: "golang.org/x/sys", Version: "v0.41.0", Targets: []string{"darwin", "linux"}, LicenseFiles: []string{"LICENSE", "PATENTS"}},
	{Module: "github.com/tklauser/go-sysconf", Version: "v0.3.16", Targets: []string{"darwin", "linux"}, LicenseFiles: []string{"LICENSE"}},
	{Module: "github.com/ebitengine/purego", Version: "v0.10.0", Targets: []string{"darwin"}, LicenseFiles: []string{"LICENSE"}},
	{Module: "github.com/tklauser/numcpus", Version: "v0.11.0", Targets: []string{"linux"}, LicenseFiles: []string{"LICENSE"}},
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

// Render renders the third-party notice document from a module cache path.
func Render(moduleCache string) ([]byte, error) {
	var b strings.Builder
	for i, dep := range Inventory() {
		b.WriteString("## ")
		b.WriteString(dep.Module)
		b.WriteByte(' ')
		b.WriteString(dep.Version)
		b.WriteByte('\n')
		b.WriteString("Targets: ")
		b.WriteString(strings.Join(dep.Targets, ", "))
		b.WriteByte('\n')
		b.WriteByte('\n')

		for _, fileName := range dep.LicenseFiles {
			path := filepath.Join(moduleCache, filepath.FromSlash(dep.Module+"@"+dep.Version), fileName)
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read %s %s %s: %w", dep.Module, dep.Version, fileName, err)
			}

			b.WriteString("### ")
			b.WriteString(fileName)
			b.WriteByte('\n')
			b.WriteByte('\n')
			b.Write(data)
			if len(data) == 0 || data[len(data)-1] != '\n' {
				b.WriteByte('\n')
			}
			b.WriteByte('\n')
		}

		if i+1 < len(fixedInventory) {
			b.WriteByte('\n')
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

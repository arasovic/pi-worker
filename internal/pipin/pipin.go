// Package pipin reads the Pi release pin declared in compat/pi/package.json
// and propagates it to every site that records a verified Pi version: the
// VerifiedVersion constant in internal/piversion/version.go and the prose
// mentions in Markdown files. The direction is one-way: the manifest is the
// source of truth, and the constant and the prose are targets that get
// rewritten. Nothing ever reads the Go constant back and compares the
// manifest against it.
package pipin

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/arasovic/pi-worker/internal/piversion"
)

const (
	// piPackage is the dependency key carrying the pin.
	piPackage = "@earendil-works/pi-coding-agent"
	// pinPath is the manifest that declares the pin.
	pinPath = "compat/pi/package.json"
	// versionGoPath is the Go target that records the pin as a constant.
	versionGoPath = "internal/piversion/version.go"
)

// Site is one place that records the verified Pi version.
type Site struct {
	// Path is relative to the repository root.
	Path string
	// Line is the 1-based line number of the version token.
	Line int
	// Version is the bare semantic version recorded at the site.
	Version string

	// start and end bound the version token within the line so a rewrite can
	// replace only the token and leave surrounding markup untouched.
	start, end int
}

// ReadPin parses compat/pi/package.json and returns the pinned version: the
// bare semantic version of dependencies["@earendil-works/pi-coding-agent"].
func ReadPin(root string) (string, error) {
	path := filepath.Join(root, pinPath)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", pinPath, err)
	}
	var manifest struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", fmt.Errorf("parse %s: %w", pinPath, err)
	}
	version, ok := manifest.Dependencies[piPackage]
	if !ok {
		return "", fmt.Errorf("%s: dependency %q is missing", pinPath, piPackage)
	}
	if !piversion.ValidSemanticVersion(version) {
		return "", fmt.Errorf("%s: dependency %q is %q, which is not a bare semantic version", pinPath, piPackage, version)
	}
	return version, nil
}

// Sites returns every site that records the verified Pi version in
// deterministic order: the Go constant first, then Markdown files in walk
// order. The tree is walked rather than read from git, so the result does not
// depend on a git process and works the same in every job that runs the
// check.
func Sites(root string) ([]Site, error) {
	sites := make([]Site, 0, 8)
	goSite, err := siteFromVersionGo(root)
	if err != nil {
		return nil, err
	}
	sites = append(sites, goSite)

	// docs/pi-cli-surface.md is deliberately not read at all. It records what
	// was actually observed when the Pi surface was probed, including a
	// 0.84.1 stratum from an earlier probe. Those are observation records,
	// not claims about the current pin; rewriting them would falsify history.
	excluded := filepath.Join(root, "docs", "pi-cli-surface.md")

	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "node_modules" || entry.Name() == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".md") || path == excluded {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(data), "\n") {
			if site, ok := markdownSite(relative, i+1, line); ok {
				sites = append(sites, site)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sites, nil
}

// siteFromVersionGo finds the VerifiedVersion constant in
// internal/piversion/version.go. The declaration is the line that contains
// both the constant name and a quoted assignment, which survives gofmt
// alignment changes; comment and comparison lines do not carry both.
func siteFromVersionGo(root string) (Site, error) {
	path := filepath.Join(root, versionGoPath)
	data, err := os.ReadFile(path)
	if err != nil {
		return Site{}, fmt.Errorf("read %s: %w", versionGoPath, err)
	}
	for i, line := range strings.Split(string(data), "\n") {
		name := strings.Index(line, "VerifiedVersion")
		if name < 0 {
			continue
		}
		assignment := strings.Index(line[name:], `= "`)
		if assignment < 0 {
			continue
		}
		start := name + assignment + len(`= "`)
		end := strings.IndexByte(line[start:], '"')
		if end < 0 {
			return Site{}, fmt.Errorf("%s:%d: VerifiedVersion assignment has no closing quote", versionGoPath, i+1)
		}
		end += start
		version := line[start:end]
		if !piversion.ValidSemanticVersion(version) {
			return Site{}, fmt.Errorf("%s:%d: VerifiedVersion is %q, which is not a bare semantic version", versionGoPath, i+1, version)
		}
		return Site{Path: versionGoPath, Line: i + 1, Version: version, start: start, end: end}, nil
	}
	return Site{}, fmt.Errorf("%s: no VerifiedVersion constant declared", versionGoPath)
}

// markdownSite finds the version site on one Markdown line, if any. A site is
// a bare semantic version token on a line where the word Pi appears earlier
// on that same line. The ordering is load-bearing: README.md contains
// "22.20.0" followed by "[Pi]" on one line, and requiring Pi to come first
// excludes it. A pattern that only required Pi and a version on the same
// line would corrupt that line.
func markdownSite(path string, lineNumber int, line string) (Site, bool) {
	pi := strings.Index(line, "Pi")
	if pi < 0 {
		return Site{}, false
	}
	version, start, ok := firstSemver(line, pi+len("Pi"))
	if !ok {
		return Site{}, false
	}
	return Site{Path: path, Line: lineNumber, Version: version, start: start, end: start + len(version)}, true
}

// firstSemver returns the first bare semantic version token at or after start
// with its byte range in the line. A token is a maximal run of version
// characters, and the whole run must satisfy the SemVer 2.0.0 grammar, so the
// token is not anchored to surrounding punctuation: real sites are wrapped in
// backticks, double asterisks, or nothing at all.
func firstSemver(line string, start int) (string, int, bool) {
	for i := start; i < len(line); i++ {
		if line[i] < '0' || line[i] > '9' {
			continue
		}
		j := i
		for j < len(line) && isVersionCharacter(line[j]) {
			j++
		}
		if piversion.ValidSemanticVersion(line[i:j]) {
			return line[i:j], i, true
		}
	}
	return "", 0, false
}

// Check compares every site against the pin and returns one report line per
// site that disagrees, naming the file, the line number, the version found,
// and the pin. The slice is empty when every site agrees with the pin.
func Check(root string) ([]string, error) {
	pin, err := ReadPin(root)
	if err != nil {
		return nil, err
	}
	sites, err := Sites(root)
	if err != nil {
		return nil, err
	}
	var reports []string
	for _, site := range sites {
		if site.Version != pin {
			reports = append(reports, fmt.Sprintf("%s:%d: version %s, pin is %s", site.Path, site.Line, site.Version, pin))
		}
	}
	return reports, nil
}

// Write propagates the pin to every site that disagrees with it and returns
// the rewritten sites. A second call on the same tree rewrites nothing.
func Write(root string) ([]Site, error) {
	pin, err := ReadPin(root)
	if err != nil {
		return nil, err
	}
	sites, err := Sites(root)
	if err != nil {
		return nil, err
	}
	changed := make([]Site, 0, len(sites))
	for _, site := range sites {
		if site.Version == pin {
			continue
		}
		if err := rewriteSite(root, site, pin); err != nil {
			return nil, err
		}
		changed = append(changed, site)
	}
	return changed, nil
}

// rewriteSite replaces the version token on one line of one file with the
// pin, preserving the rest of the file byte for byte.
func rewriteSite(root string, site Site, pin string) error {
	path := filepath.Join(root, site.Path)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	if site.Line < 1 || site.Line > len(lines) {
		return fmt.Errorf("%s:%d: line out of range", site.Path, site.Line)
	}
	line := lines[site.Line-1]
	if site.end > len(line) || line[site.start:site.end] != site.Version {
		return fmt.Errorf("%s:%d: line changed since it was scanned; refusing to rewrite", site.Path, site.Line)
	}
	lines[site.Line-1] = line[:site.start] + pin + line[site.end:]
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}

// isVersionCharacter reports whether c can appear inside a semantic version
// token. Prose punctuation such as backticks, asterisks, commas, and closing
// brackets is not a version character, so a token ends exactly at the markup
// boundary.
func isVersionCharacter(c byte) bool {
	return (c >= '0' && c <= '9') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= 'a' && c <= 'z') ||
		c == '.' || c == '-' || c == '+'
}

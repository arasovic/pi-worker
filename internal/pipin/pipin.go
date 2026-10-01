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
	"sort"
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
// check. The walk skips .git, node_modules, dist, and the repository-root
// .pi-worker directory (and everything beneath it), and ignores
// docs/pi-cli-surface.md.
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
	// The repository-root .pi-worker directory is also skipped entirely: it
	// holds managed linked checkouts that are ignored by Git and must not be
	// treated as repository content. The skip is rooted at join(root,
	// ".pi-worker") so an unrelated nested directory merely named .pi-worker
	// elsewhere in the tree is still scanned.
	excluded := filepath.Join(root, "docs", "pi-cli-surface.md")
	piWorkerRoot := filepath.Join(root, ".pi-worker")

	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path == piWorkerRoot {
				return filepath.SkipDir
			}
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
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			next := ""
			hasNext := i+1 < len(lines)
			if hasNext {
				next = lines[i+1]
			}
			sites = append(sites, markdownSites(relative, i+1, line, next, hasNext)...)
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

// markdownSites returns every version site recorded on one Markdown line. A
// site is a bare semantic version token after the word Pi on the line, and
// every such token counts: a line may carry more than one. A line whose last
// word is Pi also looks for a version that begins the next line, so prose
// rewrapped with Pi ending one line and the version starting the next is
// still a site, reported on the line that writes the version. The ordering is
// load-bearing: README.md contains "22.20.0" followed by "[Pi]" on one line,
// and requiring Pi to come first excludes it. A pattern that only required Pi
// and a version on the same line would corrupt that line.
func markdownSites(path string, lineNumber int, line, next string, hasNext bool) []Site {
	var sites []Site
	if pi := strings.Index(line, "Pi"); pi >= 0 {
		for _, site := range semverTokens(line, pi+len("Pi")) {
			site.Path = path
			site.Line = lineNumber
			sites = append(sites, site)
		}
	}
	if hasNext && endsWithPiWord(line) {
		if version, start, ok := firstVersionOfLine(next); ok {
			sites = append(sites, Site{Path: path, Line: lineNumber + 1, Version: version, start: start, end: start + len(version)})
		}
	}
	return sites
}

// semverTokens returns every bare semantic version token at or after start
// with its byte range in the line. A token is a maximal run of version
// characters, and the whole run must satisfy the SemVer 2.0.0 grammar, so the
// token is not anchored to surrounding punctuation: real sites are wrapped in
// backticks, double asterisks, or nothing at all.
func semverTokens(line string, start int) []Site {
	var tokens []Site
	for i := start; i < len(line); i++ {
		if line[i] < '0' || line[i] > '9' {
			continue
		}
		j := i
		for j < len(line) && isVersionCharacter(line[j]) {
			j++
		}
		if piversion.ValidSemanticVersion(line[i:j]) {
			tokens = append(tokens, Site{Version: line[i:j], start: i, end: j})
			i = j - 1
		}
	}
	return tokens
}

// endsWithPiWord reports whether the last word of the line is Pi, ignoring
// trailing whitespace and Markdown punctuation such as "**", "]", or a
// backtick. A wrapped pair is one where Pi ends a line and the version begins
// the next.
func endsWithPiWord(line string) bool {
	end := len(line)
	for end > 0 && isLineSpaceOrPunctuation(line[end-1]) {
		end--
	}
	if end < len("Pi") || line[end-len("Pi"):end] != "Pi" {
		return false
	}
	before := end - len("Pi")
	if before == 0 {
		return true
	}
	return isLineSpaceOrPunctuation(line[before-1])
}

// firstVersionOfLine returns the version token that is the first word of the
// line, after skipping leading whitespace, Markdown punctuation, and any list
// marker. It is the second half of the wrapped-site rule.
func firstVersionOfLine(line string) (string, int, bool) {
	i := skipLeadingMarkdown(line)
	// An ordered list marker is digits followed by "." or ")" and a space.
	j := i
	for j < len(line) && line[j] >= '0' && line[j] <= '9' {
		j++
	}
	if j > i && j+1 < len(line) && (line[j] == '.' || line[j] == ')') && (line[j+1] == ' ' || line[j+1] == '\t') {
		i = j + 1 + skipLeadingMarkdown(line[j+1:])
	}
	if i >= len(line) || line[i] < '0' || line[i] > '9' {
		return "", 0, false
	}
	end := i
	for end < len(line) && isVersionCharacter(line[end]) {
		end++
	}
	version := line[i:end]
	if !piversion.ValidSemanticVersion(version) {
		return "", 0, false
	}
	return version, i, true
}

// skipLeadingMarkdown returns the offset of the first byte of line that is not
// leading whitespace or Markdown punctuation.
func skipLeadingMarkdown(line string) int {
	i := 0
	for i < len(line) && isLineSpaceOrPunctuation(line[i]) {
		i++
	}
	return i
}

// isLineSpaceOrPunctuation reports whether c is whitespace that may trail a
// line or ASCII punctuation that can decorate a Markdown token. Version
// characters are never punctuation.
func isLineSpaceOrPunctuation(c byte) bool {
	if c == ' ' || c == '\t' || c == '\r' {
		return true
	}
	switch c {
	case '!', '"', '#', '$', '%', '&', '\'', '(', ')', '*', '+', ',', '-', '.', '/', ':', ';', '<', '=', '>', '?', '@', '[', '\\', ']', '^', '_', '`', '{', '|', '}', '~':
		return true
	}
	return false
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
	byFile := map[string][]Site{}
	var order []string
	for _, site := range sites {
		if site.Version == pin {
			continue
		}
		if _, seen := byFile[site.Path]; !seen {
			order = append(order, site.Path)
		}
		byFile[site.Path] = append(byFile[site.Path], site)
		changed = append(changed, site)
	}
	for _, path := range order {
		if err := rewriteSites(root, path, byFile[path], pin); err != nil {
			return nil, err
		}
	}
	return changed, nil
}

// rewriteSites replaces every version token on the named file with the pin,
// preserving the rest of the file byte for byte. Tokens are applied to each
// line from the end backwards, so replacing a longer token with a shorter one
// never shifts the offsets of an earlier token still to be replaced on the
// same line.
func rewriteSites(root, path string, sites []Site, pin string) error {
	data, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	byLine := map[int][]Site{}
	for _, site := range sites {
		byLine[site.Line] = append(byLine[site.Line], site)
	}
	lineNumbers := make([]int, 0, len(byLine))
	for line := range byLine {
		lineNumbers = append(lineNumbers, line)
	}
	sort.Ints(lineNumbers)
	for _, lineNumber := range lineNumbers {
		if lineNumber < 1 || lineNumber > len(lines) {
			return fmt.Errorf("%s:%d: line out of range", path, lineNumber)
		}
		line := lines[lineNumber-1]
		candidates := byLine[lineNumber]
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].start > candidates[j].start })
		for _, site := range candidates {
			if site.end > len(line) || line[site.start:site.end] != site.Version {
				return fmt.Errorf("%s:%d: line changed since it was scanned; refusing to rewrite", path, site.Line)
			}
			line = line[:site.start] + pin + line[site.end:]
		}
		lines[lineNumber-1] = line
	}
	return os.WriteFile(filepath.Join(root, path), []byte(strings.Join(lines, "\n")), 0o644)
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

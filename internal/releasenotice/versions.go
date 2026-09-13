package releasenotice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// versionFormat is the `go list` template reporting a module and the version
// the build selects for it. A module named by a replace directive is built at
// the replacement's version, so the template reports the replacement's path and
// version whenever one is set; the resolver refuses a replacement it cannot
// describe honestly. Without a replace directive only the module's own version
// follows its path, so an unreplaced module answers with exactly two fields.
const versionFormat = "{{.Path}} {{if .Replace}}{{.Replace.Path}} {{.Replace.Version}}{{else}}{{.Version}}{{end}}"

// errNoSelectedVersion is reported for a declared module whose version the
// build did not supply. It is the one failure that would otherwise render a
// blank version into the notice document.
var errNoSelectedVersion = errors.New("no version selected by the go build")

// goRun is one run of the go command: its streams and its exit status.
type goRun struct {
	stdout string
	stderr string
}

// SelectedVersions returns the version the build selects for every module in
// Inventory, read from the go command that governs this package's own module.
//
// The versions named in the notice document are the versions the binary
// contains, so they are resolved, not declared. A module the go command does
// not answer for is an error naming that module: a version never resolves to
// the empty string, so a dependency bump cannot ship a notice that names a
// version no build uses.
func SelectedVersions() (map[string]string, error) {
	modules := make([]string, 0, len(fixedInventory))
	for _, dep := range fixedInventory {
		modules = append(modules, dep.Module)
	}
	return selectedVersions(context.Background(), modules)
}

// selectedVersions asks the go command for the version it selects for each of
// the given modules, in one run, and fails if any of them goes unanswered.
func selectedVersions(ctx context.Context, modules []string) (map[string]string, error) {
	versions := make(map[string]string, len(modules))
	if len(modules) == 0 {
		return versions, nil
	}

	root, err := moduleRoot()
	if err != nil {
		return nil, err
	}

	args := append([]string{"list", "-m", "-f", versionFormat}, modules...)
	run, err := goList(ctx, root, args...)
	if err != nil {
		return nil, fmt.Errorf("%w: go %s in %s failed: %v: %s",
			errNoSelectedVersion, strings.Join(args, " "), root, err, goComplaint(run.stderr))
	}

	selected := parseSelections(run.stdout)

	for _, module := range modules {
		selection, ok := selected[module]
		if !ok {
			return nil, fmt.Errorf("%w: module %s is not in the build list of %s: go %s answered: %s",
				errNoSelectedVersion, module, root, strings.Join(args, " "), goComplaint(run.stderr))
		}
		if selection.replacementPath != "" {
			if selection.version == "" {
				return nil, fmt.Errorf("%w: module %s is replaced by the local directory %s by a replace directive, for which the go command selects no version",
					errNoSelectedVersion, module, selection.replacementPath)
			}
			if selection.replacementPath != module {
				return nil, fmt.Errorf("%w: module %s is replaced by %s, a different module, by a replace directive, whose license lives under another module's cache directory",
					errNoSelectedVersion, module, selection.replacementPath)
			}
		}
		if selection.version == "" {
			return nil, fmt.Errorf("%w: module %s was reported with an empty version by go in %s",
				errNoSelectedVersion, module, root)
		}
		versions[module] = selection.version
	}
	return versions, nil
}

// moduleSelection is what the go command answered for one module: the version
// the build selects for it, and the path a replace directive sends it to when
// one is set.
type moduleSelection struct {
	replacementPath string
	version         string
}

// parseSelections reads the lines versionFormat produces. A module without a
// replace directive answers with its own version; a replaced module answers
// with the replacement's path and version, the version empty when the
// replacement is a local directory that has none. Module paths and versions
// hold no spaces, so the number of fields says which answer a line is.
func parseSelections(stdout string) map[string]moduleSelection {
	selected := make(map[string]moduleSelection)
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Split(line, " ")
		switch {
		case len(fields) == 2 && fields[0] != "":
			selected[fields[0]] = moduleSelection{version: fields[1]}
		case len(fields) == 3 && fields[0] != "":
			selected[fields[0]] = moduleSelection{replacementPath: fields[1], version: fields[2]}
		}
	}
	return selected
}

// goList runs the go command in dir and returns the run. It is a variable so a
// test can stand in for an environment where the go command answers nothing;
// production callers reach it only through SelectedVersions.
var goList = func(ctx context.Context, dir string, args ...string) (goRun, error) {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return goRun{stdout: stdout.String(), stderr: stderr.String()}, err
}

// moduleRoot returns the directory holding the go.mod of the module containing
// the working directory. That module's build list is what the notice document
// has to describe.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("determine the working directory: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("find the module the notices describe: no go.mod above %q", dir)
		}
		dir = parent
	}
}

// goComplaint reduces go's stderr to the line that explains a failure, dropping
// the download and work poll chatter a cold module cache reports on the way.
func goComplaint(stderr string) string {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "go: downloading ") {
			continue
		}
		return line
	}
	return "the go command failed without an explanation"
}

// selectedVersion returns module's resolved version, or an error naming it.
func selectedVersion(versions map[string]string, module string) (string, error) {
	version, ok := versions[module]
	if !ok || version == "" {
		return "", fmt.Errorf("%w: module %s", errNoSelectedVersion, module)
	}
	return version, nil
}

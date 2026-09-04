package run

import (
	"path/filepath"
	"sort"
	"strings"
)

// WriteCheck is the post-hoc comparison of the paths a run actually
// changed against the paths its tasks declared they would write: the
// changed paths no task declared, reported after a terminal status. It
// is accounting, not enforcement: nothing was blocked, rolled back, or
// stopped, and the comparison runs only after the run has ended.
//
// Skipped carries the reason the check could not run; when it is
// present the other three fields carry no meaning. Otherwise
// UndeclaredCount is the true number of changed paths no task declared
// — never absent, so a checked run that wrote nothing undeclared
// carries a present zero and stays distinguishable from a run that was
// never checked — and Undeclared lists those paths capped at
// maxChangeFiles entries, with Truncated true only when the cap dropped
// entries.
type WriteCheck struct {
	Skipped         string   `json:"skipped,omitempty"`
	Undeclared      []string `json:"undeclared,omitempty"`
	UndeclaredCount int      `json:"undeclaredCount"`
	Truncated       bool     `json:"truncated,omitempty"`
}

// The write-check skip reason, a short lowercase phrase in the style of
// the change-manifest omission reasons. Only one reason remains: a
// partial declaration is rejected before the run ever starts, so a
// check that runs can only be skipped by a manifest it never got to
// compare against.
const (
	reasonManifestUnavailable = "change manifest unavailable"
)

// anyWritesDeclared reports whether at least one task carried a write
// declaration — the run-level "did the caller declare at all" bit the
// controller uses to decide whether the check runs at all. A task that
// declared an empty set has declared, so it counts; the bit is about the
// declaration happening at all, never about how many paths it named. It
// must not degrade into a len(Paths) test: the writes-nothing declaration
// carries zero paths and still declares.
func anyWritesDeclared(tasks []Task) bool {
	for _, task := range tasks {
		if task.Writes.Declared {
			return true
		}
	}
	return false
}

// checkWrites compares the paths a run changed, as recorded in the
// change manifest, against every task's declared write paths pooled
// together, and returns the write check. A verdict or a skip reason is
// always returned; the caller decides whether the check runs at all by
// whether at least one task carried a write declaration. The comparison
// is pure over two in-memory slices: it runs no commands, so it takes no
// context and has no timeout of its own. workspace is the coordinate
// system of the declarations; the boundary translates them to the
// manifest's repository-root coordinates before comparing.
func checkWrites(changes *Changes, tasks []Task, workspace string) *WriteCheck {
	return checkWritesWithExtra(changes, tasks, workspace, nil)
}

func checkWritesWithExtra(changes *Changes, tasks []Task, workspace string, extraUndeclared []string) *WriteCheck {
	if changes == nil || changes.Omitted != "" {
		if len(extraUndeclared) > 0 {
			sorted := make([]string, len(extraUndeclared))
			copy(sorted, extraUndeclared)
			sort.Strings(sorted)
			sorted = dedupSortedPaths(sorted)
			return writeCheckFromPaths(sorted)
		}
		return &WriteCheck{Skipped: reasonManifestUnavailable}
	}
	declared := make([]string, 0)
	for _, task := range tasks {
		for _, path := range task.Writes.Paths {
			declared = append(declared, reanchorWritePath(changes.root, workspace, path))
		}
	}
	var undeclared []string
	for _, path := range changes.allPaths {
		if !pathDeclared(declared, path) {
			undeclared = append(undeclared, path)
		}
	}
	merged := mergeUndeclaredPaths(undeclared, extraUndeclared)
	return writeCheckFromPaths(merged)
}

func mergeUndeclaredPaths(base, extra []string) []string {
	if len(extra) == 0 {
		sorted := make([]string, len(base))
		copy(sorted, base)
		sort.Strings(sorted)
		return dedupSortedPaths(sorted)
	}
	combined := make([]string, 0, len(base)+len(extra))
	combined = append(combined, base...)
	combined = append(combined, extra...)
	sort.Strings(combined)
	return dedupSortedPaths(combined)
}

func dedupSortedPaths(sorted []string) []string {
	if len(sorted) == 0 {
		return nil
	}
	deduped := make([]string, 0, len(sorted))
	for _, p := range sorted {
		if len(deduped) == 0 || deduped[len(deduped)-1] != p {
			deduped = append(deduped, p)
		}
	}
	return deduped
}

func writeCheckFromPaths(paths []string) *WriteCheck {
	check := &WriteCheck{UndeclaredCount: len(paths)}
	if len(paths) > maxChangeFiles {
		check.Undeclared = paths[:maxChangeFiles]
		check.Truncated = true
	} else if len(paths) > 0 {
		check.Undeclared = paths
	}
	return check
}

func projectedTaskStamps(stamps map[string]fileStamp, task Task, root, workspace string) map[string]fileStamp {
	if stamps == nil {
		return nil
	}
	if !task.Writes.Declared {
		return map[string]fileStamp{}
	}
	declared := make([]string, 0, len(task.Writes.Paths))
	for _, p := range task.Writes.Paths {
		declared = append(declared, reanchorWritePath(root, workspace, p))
	}
	out := make(map[string]fileStamp, len(stamps))
	for path, stamp := range stamps {
		if pathDeclared(declared, path) {
			out[path] = stamp
		}
	}
	return out
}

func fileStampEqual(a, b fileStamp) bool {
	if a.size != b.size || a.exec != b.exec || a.absent != b.absent || a.dir != b.dir || a.gitMarker != b.gitMarker || a.hashed != b.hashed {
		return false
	}
	if !a.modTime.Equal(b.modTime) {
		return false
	}
	if a.hashed && a.contentHash != b.contentHash {
		return false
	}
	if (a.descendants == nil) != (b.descendants == nil) {
		return false
	}
	if len(a.descendants) != len(b.descendants) {
		return false
	}
	for i := range a.descendants {
		if a.descendants[i] != b.descendants[i] {
			return false
		}
	}
	return true
}

func diffProjectedStamps(settled, final map[string]fileStamp) []string {
	keys := make(map[string]bool, len(settled)+len(final))
	for k := range settled {
		keys[k] = true
	}
	for k := range final {
		keys[k] = true
	}
	var diff []string
	for path := range keys {
		a, aOk := settled[path]
		b, bOk := final[path]
		if !aOk || !bOk {
			diff = append(diff, path)
			continue
		}
		if !fileStampEqual(a, b) {
			diff = append(diff, path)
		}
	}
	sort.Strings(diff)
	return diff
}

// writesDeclaredOnEveryTask reports whether every task carried a write
// declaration, the all-or-none condition ValidateWrites enforces: a run
// where some tasks declared and others did not is rejected before any
// worker starts, while a run where every task declared — the empty set
// included — or none did stays legal. A task that declared an empty set
// has declared, so a writes-nothing task counts; the empty declaration
// list counts as none declared, which is why anyWritesDeclared guards
// the rejection in ValidateWrites.
func writesDeclaredOnEveryTask(tasks []Task) bool {
	if len(tasks) == 0 {
		return false
	}
	for _, task := range tasks {
		if !task.Writes.Declared {
			return false
		}
	}
	return true
}

// reanchorWritePath translates one workspace-relative declaration into
// a repository-root-relative path, the coordinate system used by the
// change manifest. It is deliberately done at the shared write-check
// boundary so validation and the public workspace-relative contract stay
// unchanged.
func reanchorWritePath(root, workspace, path string) string {
	if root == "" || workspace == "" {
		return path
	}
	var err error
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return path
	}
	workspace, err = filepath.Abs(workspace)
	if err != nil {
		return path
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return path
	}
	anchored, err := filepath.Rel(root, filepath.Join(workspace, filepath.Clean(path)))
	if err != nil {
		return path
	}
	return filepath.Clean(anchored)
}

// pathDeclared reports whether any declared path covers changed: the
// directional form of the declaration, a declared path covering a
// changed path, which is the shape of the check rather than the
// symmetric overlap validation uses. Both sides are normalised to
// forward slashes before comparing, because manifest paths come from
// git already in that form while declared paths went through
// filepath.Clean and use the platform separator.
func pathDeclared(declared []string, changed string) bool {
	changed = filepath.ToSlash(changed)
	for _, d := range declared {
		if declaredPathCovers(filepath.ToSlash(filepath.Clean(d)), changed) {
			return true
		}
	}
	return false
}

// declaredPathCovers reports whether declared is equal to changed or a
// path-prefix of changed on a segment boundary: "src/a" covers
// "src/a/b.go" and does not cover "src/ab.go". Both sides are already
// normalised to forward slashes.
func declaredPathCovers(declared, changed string) bool {
	if declared == changed {
		return true
	}
	dseg := strings.Split(declared, "/")
	cseg := strings.Split(changed, "/")
	if len(dseg) > len(cseg) {
		return false
	}
	for i := range dseg {
		if dseg[i] != cseg[i] {
			return false
		}
	}
	return true
}

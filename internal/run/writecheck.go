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

// The write-check skip reasons, short lowercase phrases in the style of
// the change-manifest omission reasons. When both apply the first is
// reported: it is the caller's own input, and it is the one they can
// act on.
const (
	reasonPartialDeclaration  = "not all tasks declared writes"
	reasonManifestUnavailable = "change manifest unavailable"
)

// checkWrites compares the paths a run changed, as recorded in the
// change manifest, against every task's declared write paths pooled
// together, and returns the write check. A verdict or a skip reason is
// always returned; the caller decides whether the check runs at all by
// whether it passed a Writes declaration. The comparison is pure over
// two in-memory slices: it runs no commands, so it takes no context and
// has no timeout of its own.
func checkWrites(changes *Changes, writes []WriteDeclaration) *WriteCheck {
	if !writesDeclaredOnEveryTask(writes) {
		return &WriteCheck{Skipped: reasonPartialDeclaration}
	}
	if changes == nil || changes.Omitted != "" {
		return &WriteCheck{Skipped: reasonManifestUnavailable}
	}
	declared := make([]string, 0)
	for _, task := range writes {
		declared = append(declared, task.Paths...)
	}
	var undeclared []string
	for _, path := range changes.allPaths {
		if !pathDeclared(declared, path) {
			undeclared = append(undeclared, path)
		}
	}
	sort.Strings(undeclared)
	check := &WriteCheck{UndeclaredCount: len(undeclared)}
	if len(undeclared) > maxChangeFiles {
		check.Undeclared = undeclared[:maxChangeFiles]
		check.Truncated = true
	} else if len(undeclared) > 0 {
		check.Undeclared = undeclared
	}
	return check
}

// writesDeclaredOnEveryTask reports whether every task carried a write
// declaration, exactly the condition the CLI's shared-workspace warning
// uses. A task that declared nothing may legitimately have written any
// path, so no changed path in the run can be called undeclared; a task
// that declared an empty set has declared — the task bounded itself to
// nothing — and the check runs. An empty declaration list, no task
// declaring anything, is the same partial state as a task that said
// nothing.
func writesDeclaredOnEveryTask(writes []WriteDeclaration) bool {
	if len(writes) == 0 {
		return false
	}
	for _, task := range writes {
		if !task.Declared {
			return false
		}
	}
	return true
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

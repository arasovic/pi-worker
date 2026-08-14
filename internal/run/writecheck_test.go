package run

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/contracts"
	"github.com/arasovic/pi-worker/internal/pi"
)

// runWithWrites drives dir through the controller with the real git
// inspector and the given per-task write declarations, failing the test
// on any controller error. Files the worker creates land inside the run
// so the before-tree stays clean and the manifest gets measured.
func runWithWrites(t *testing.T, worker pi.Worker, dir string, tasks []string, writes [][]string) Result {
	t.Helper()
	req := validRequest(tasks...)
	req.Workspace = dir
	req.Writes = writes
	result, err := New(worker, WithGitInspector(NewDefaultGitInspector())).Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return result
}

// writeCheckDocument marshals check and returns the decoded document,
// so tests can assert the exact serialized key set.
func writeCheckDocument(t *testing.T, check *WriteCheck) map[string]any {
	t.Helper()
	data, err := json.Marshal(check)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return document
}

// assertExactJSONKeys fails unless document carries exactly the given
// keys.
func assertExactJSONKeys(t *testing.T, document map[string]any, want ...string) {
	t.Helper()
	if len(document) != len(want) {
		t.Fatalf("document keys = %v, want %v", documentKeys(document), want)
	}
	for _, key := range want {
		if _, present := document[key]; !present {
			t.Fatalf("document missing key %q: %v", key, document)
		}
	}
}

func documentKeys(document map[string]any) []string {
	keys := make([]string, 0, len(document))
	for key := range document {
		keys = append(keys, key)
	}
	return keys
}

func TestControllerWritesInsideDeclarationClean(t *testing.T) {
	dir := newGitRepo(t)
	result := runWithWrites(t, &changesMutatingWorker{mutate: func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "file.txt"), []byte("changed\n"), 0o644)
	}}, dir, []string{"a"}, [][]string{{"file.txt"}})
	writes := result.Writes
	if writes == nil {
		t.Fatalf("writes = nil, want a verdict on an in-declaration run")
	}
	if writes.Skipped != "" || writes.UndeclaredCount != 0 || len(writes.Undeclared) != 0 || writes.Truncated {
		t.Fatalf("writes = %#v, want checked-clean", writes)
	}
	// The presence discipline: a checked run serializes undeclaredCount
	// as a present zero, never as an absent field, and carries neither
	// the empty undeclared list, a false truncation, nor a skip reason.
	document := writeCheckDocument(t, writes)
	assertExactJSONKeys(t, document, "undeclaredCount")
	if document["undeclaredCount"] != float64(0) {
		t.Fatalf("undeclaredCount = %v, want present 0", document["undeclaredCount"])
	}
}

func TestControllerWritesUndeclaredPathReported(t *testing.T) {
	dir := newGitRepo(t)
	result := runWithWrites(t, &changesMutatingWorker{mutate: func(dir string) error {
		if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "src", "a.txt"), []byte("a\n"), 0o644); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "src", "stray.txt"), []byte("stray\n"), 0o644)
	}}, dir, []string{"a"}, [][]string{{"src/a.txt"}})
	writes := result.Writes
	if writes == nil || writes.Skipped != "" {
		t.Fatalf("writes = %#v, want a verdict", writes)
	}
	if writes.UndeclaredCount != 1 || len(writes.Undeclared) != 1 || writes.Undeclared[0] != "src/stray.txt" || writes.Truncated {
		t.Fatalf("writes = %#v, want exactly src/stray.txt undeclared", writes)
	}
	document := writeCheckDocument(t, writes)
	assertExactJSONKeys(t, document, "undeclared", "undeclaredCount")
	if document["undeclaredCount"] != float64(1) {
		t.Fatalf("undeclaredCount = %v, want 1", document["undeclaredCount"])
	}
}

func TestControllerWritesDirectoryDeclarationCoversBeneath(t *testing.T) {
	dir := newGitRepo(t)
	result := runWithWrites(t, &changesMutatingWorker{mutate: func(dir string) error {
		if err := os.MkdirAll(filepath.Join(dir, "src", "a"), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "src", "a", "b.go"), []byte("package a\n"), 0o644)
	}}, dir, []string{"a"}, [][]string{{"src/a"}})
	writes := result.Writes
	if writes == nil || writes.Skipped != "" {
		t.Fatalf("writes = %#v, want a verdict", writes)
	}
	if writes.UndeclaredCount != 0 || len(writes.Undeclared) != 0 || writes.Truncated {
		t.Fatalf("writes = %#v, want the file beneath the declared directory covered", writes)
	}
}

func TestControllerWritesPrefixWithoutSegmentBoundaryUndeclared(t *testing.T) {
	dir := newGitRepo(t)
	result := runWithWrites(t, &changesMutatingWorker{mutate: func(dir string) error {
		if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "src", "ab.go"), []byte("package ab\n"), 0o644)
	}}, dir, []string{"a"}, [][]string{{"src/a"}})
	writes := result.Writes
	if writes == nil || writes.Skipped != "" {
		t.Fatalf("writes = %#v, want a verdict", writes)
	}
	// Declaring src/a must not cover src/ab.go: "a" is a string prefix
	// but not a segment.
	if writes.UndeclaredCount != 1 || len(writes.Undeclared) != 1 || writes.Undeclared[0] != "src/ab.go" || writes.Truncated {
		t.Fatalf("writes = %#v, want exactly src/ab.go undeclared", writes)
	}
}

func TestControllerWritesAbsentWithoutDeclaration(t *testing.T) {
	dir := newGitRepo(t)
	result := runWithWrites(t, &changesMutatingWorker{mutate: func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "file.txt"), []byte("changed\n"), 0o644)
	}}, dir, []string{"a"}, nil)
	if result.Writes != nil {
		t.Fatalf("writes = %#v, want nil when the request declared nothing", result.Writes)
	}
}

func TestControllerWritesPartialDeclarationSkips(t *testing.T) {
	dir := newGitRepo(t)
	result := runWithWrites(t, &changesMutatingWorker{mutate: func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "file.txt"), []byte("changed\n"), 0o644)
	}}, dir, []string{"a", "b"}, [][]string{{"file.txt"}, nil})
	writes := result.Writes
	if writes == nil {
		t.Fatalf("writes = nil, want the partial-declaration skip reason")
	}
	if writes.Skipped != reasonPartialDeclaration {
		t.Fatalf("skipped = %q, want %q", writes.Skipped, reasonPartialDeclaration)
	}
	if writes.UndeclaredCount != 0 || writes.Undeclared != nil || writes.Truncated {
		t.Fatalf("writes = %#v, want no fields alongside the skip reason", writes)
	}
	// UndeclaredCount carries no omitempty, so the serialized document
	// still carries its meaningless zero beside the reason; the reason
	// is the field a caller must read first.
	document := writeCheckDocument(t, writes)
	assertExactJSONKeys(t, document, "skipped", "undeclaredCount")
	if document["skipped"] != reasonPartialDeclaration {
		t.Fatalf("skipped = %v, want %q", document["skipped"], reasonPartialDeclaration)
	}
	if document["undeclaredCount"] != float64(0) {
		t.Fatalf("undeclaredCount = %v, want meaningless zero", document["undeclaredCount"])
	}
}

func TestControllerWritesUnavailableWithoutManifest(t *testing.T) {
	// A dirty before-state omits the manifest, and a check with no
	// manifest has nothing to compare against; the caller still gets
	// the field, carrying the reason.
	dir := newGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	result := runWithWrites(t, newScriptedWorker(), dir, []string{"a"}, [][]string{{"file.txt"}})
	if result.Changes == nil || result.Changes.Omitted != reasonDirtyBeforeState {
		t.Fatalf("changes = %#v, want the dirty before-state omission", result.Changes)
	}
	writes := result.Writes
	if writes == nil {
		t.Fatalf("writes = nil, want the manifest-unavailable skip reason")
	}
	if writes.Skipped != reasonManifestUnavailable {
		t.Fatalf("skipped = %q, want %q", writes.Skipped, reasonManifestUnavailable)
	}
	if writes.UndeclaredCount != 0 || writes.Undeclared != nil || writes.Truncated {
		t.Fatalf("writes = %#v, want no fields alongside the skip reason", writes)
	}
	// UndeclaredCount carries no omitempty, so the serialized document
	// still carries its meaningless zero beside the reason; the reason
	// is the field a caller must read first.
	document := writeCheckDocument(t, writes)
	assertExactJSONKeys(t, document, "skipped", "undeclaredCount")
	if document["skipped"] != reasonManifestUnavailable {
		t.Fatalf("skipped = %v, want %q", document["skipped"], reasonManifestUnavailable)
	}
	if document["undeclaredCount"] != float64(0) {
		t.Fatalf("undeclaredCount = %v, want meaningless zero", document["undeclaredCount"])
	}
}

func TestControllerWritesFindsUndeclaredBeyondManifestCap(t *testing.T) {
	// The manifest caps its kept entries at maxChangeFiles and drops
	// untracked files beyond the cap without measuring them; the check
	// must still see every changed path, including one that appears
	// only beyond the cap. The files are created inside the worker, so
	// the before-tree stays clean and the manifest is measured instead
	// of omitted — creating them first would silently turn this test
	// into a test of the skip path.
	dir := newGitRepo(t)
	declared := make([]string, 0, maxChangeFiles)
	for i := 0; i < maxChangeFiles; i++ {
		declared = append(declared, fmt.Sprintf("a%03d.txt", i))
	}
	result := runWithWrites(t, &changesMutatingWorker{mutate: func(dir string) error {
		for i := 0; i < maxChangeFiles; i++ {
			path := filepath.Join(dir, fmt.Sprintf("a%03d.txt", i))
			if err := os.WriteFile(path, []byte("line\n"), 0o644); err != nil {
				return err
			}
		}
		return os.WriteFile(filepath.Join(dir, "z000.txt"), []byte("stray\n"), 0o644)
	}}, dir, []string{"a"}, [][]string{declared})
	changes := result.Changes
	if changes == nil || changes.Omitted != "" {
		t.Fatalf("changes = %#v, want a measured manifest", changes)
	}
	if !changes.Truncated || changes.TotalFiles != maxChangeFiles+1 || len(changes.Files) != maxChangeFiles {
		t.Fatalf("changes = %#v, want a truncated %d-path manifest", changes, maxChangeFiles+1)
	}
	writes := result.Writes
	if writes == nil || writes.Skipped != "" {
		t.Fatalf("writes = %#v, want a verdict", writes)
	}
	if writes.UndeclaredCount != 1 || len(writes.Undeclared) != 1 || writes.Undeclared[0] != "z000.txt" || writes.Truncated {
		t.Fatalf("writes = %#v, want exactly z000.txt undeclared from beyond the cap", writes)
	}
}

func TestControllerWritesUndeclaredListTruncatedAtCap(t *testing.T) {
	// More than maxChangeFiles changed paths are outside the
	// declaration: UndeclaredCount carries the true count and the list
	// itself is capped, with Truncated set. Tracked files keep the
	// measurement to one diff command, so the run stays fast.
	const total = maxChangeFiles + 110
	dir := newGitRepo(t)
	for i := 0; i < total; i++ {
		path := filepath.Join(dir, fmt.Sprintf("f%03d.txt", i))
		if err := os.WriteFile(path, []byte("one\n"), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
	}
	gitCommit(t, dir)
	declared := make([]string, 0, maxChangeFiles)
	for i := 0; i < maxChangeFiles; i++ {
		declared = append(declared, fmt.Sprintf("f%03d.txt", i))
	}
	result := runWithWrites(t, &changesMutatingWorker{mutate: func(dir string) error {
		for i := 0; i < total; i++ {
			path := filepath.Join(dir, fmt.Sprintf("f%03d.txt", i))
			if err := os.WriteFile(path, []byte("two\n"), 0o644); err != nil {
				return err
			}
		}
		return nil
	}}, dir, []string{"a"}, [][]string{declared})
	writes := result.Writes
	if writes == nil || writes.Skipped != "" {
		t.Fatalf("writes = %#v, want a verdict", writes)
	}
	const undeclared = total - maxChangeFiles
	if writes.UndeclaredCount != undeclared {
		t.Fatalf("undeclaredCount = %d, want %d", writes.UndeclaredCount, undeclared)
	}
	if !writes.Truncated {
		t.Fatalf("truncated = false, want true when the cap dropped undeclared paths")
	}
	if len(writes.Undeclared) != maxChangeFiles {
		t.Fatalf("undeclared = %d entries, want the %d cap", len(writes.Undeclared), maxChangeFiles)
	}
	// The undeclared paths sort by path, so the cap keeps the first
	// hundred of the f100..f209 range.
	if writes.Undeclared[0] != "f100.txt" {
		t.Fatalf("first undeclared = %q, want f100.txt", writes.Undeclared[0])
	}
	if writes.Undeclared[maxChangeFiles-1] != "f199.txt" {
		t.Fatalf("last undeclared = %q, want f199.txt", writes.Undeclared[maxChangeFiles-1])
	}
	document := writeCheckDocument(t, writes)
	assertExactJSONKeys(t, document, "truncated", "undeclared", "undeclaredCount")
	if document["truncated"] != true {
		t.Fatalf("truncated = %v, want true", document["truncated"])
	}
}

func TestControllerWritesCheckedOnTimedOutRun(t *testing.T) {
	// A run that stopped mid-edit is exactly the run whose stray writes
	// a caller most needs: the check runs on the terminal status with
	// the mid-edit file still in the workspace.
	dir := newGitRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	worker := &changesMutatingWorker{mutate: func(dir string) error {
		if err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("left behind\n"), 0o644); err != nil {
			return err
		}
		// Still mid-edit when the deadline fires: wait the parent out
		// instead of returning, so the run reports timed-out.
		<-ctx.Done()
		return nil
	}}
	req := validRequest("a")
	req.Workspace = dir
	req.Writes = [][]string{{"declared.txt"}}
	result, err := New(worker, WithGitInspector(NewDefaultGitInspector())).Run(ctx, req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != contracts.RunTimedOut {
		t.Fatalf("status = %q, want timed-out", result.Status)
	}
	writes := result.Writes
	if writes == nil {
		t.Fatalf("writes = nil, want a write check on a timed-out run")
	}
	if writes.Skipped != "" || writes.UndeclaredCount != 1 || len(writes.Undeclared) != 1 || writes.Undeclared[0] != "stray.txt" {
		t.Fatalf("writes = %#v, want exactly stray.txt undeclared", writes)
	}
}

func TestWriteCheckCleanVerdictForEveryAcceptedDeclaredForm(t *testing.T) {
	// The request keeps the raw declared strings; only validate's
	// discarded normalized copy was Clean'ed, so a trailing slash, a
	// ./ prefix, a doubled separator, an interior ., and a non-escaping
	// interior .. each reached the check uncleaned and each falsely
	// flagged a changed path beneath it as undeclared. The check must
	// clean its own declared inputs, so every form validation accepts
	// yields a clean verdict.
	tests := []string{
		"internal/run/",       // trailing slash
		"./internal/run",      // ./ prefix
		"internal//run",       // doubled separator
		"internal/./run",      // interior .
		"src/../internal/run", // non-escaping interior ..
	}
	for _, declared := range tests {
		t.Run(declared, func(t *testing.T) {
			check := checkWrites(&Changes{allPaths: []string{"internal/run/x.go"}}, [][]string{{declared}})
			if check.Skipped != "" || check.UndeclaredCount != 0 || len(check.Undeclared) != 0 || check.Truncated {
				t.Fatalf("writes = %#v, want checked-clean for declared %q", check, declared)
			}
		})
	}
}

func TestValidateWritePathAcceptedFormsNeverCleanToDotOrEscape(t *testing.T) {
	// The check relying on filepath.Clean of a declared path is safe
	// only because no value validateWritePath accepts can clean to "."
	// or to an escaping "..": the comparison would then compare
	// segments the declaration never meant. Assert the boundary rather
	// than reasoning about it.
	rejected := []string{
		"",            // empty
		"   ",         // whitespace-only
		"/etc/passwd", // absolute
		"../outside",  // escapes the workspace
		"a/../../outside",
		".",    // whole workspace
		"./",   // whole workspace via dot-slash
		"a/..", // cleans to the whole workspace
	}
	for _, value := range rejected {
		if _, err := validateWritePath(value); err == nil {
			t.Fatalf("validateWritePath accepted %q, want rejection", value)
		}
	}
	accepted := []string{
		"internal/run/",
		"./internal/run",
		"internal//run",
		"internal/./run",
		"src/../internal/run",
		".hidden",
		"internal/run/x.go",
	}
	for _, value := range accepted {
		clean, err := validateWritePath(value)
		if err != nil {
			t.Fatalf("validateWritePath rejected %q: %v", value, err)
		}
		if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			t.Fatalf("accepted path %q cleaned to %q, must be neither . nor an escaping ..", value, clean)
		}
	}
}

package run

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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
func runWithWrites(t *testing.T, worker pi.Worker, dir string, tasks []string, writes []WriteDeclaration) Result {
	t.Helper()
	req := validRequest(tasks...)
	req.Workspace = dir
	// The declaration pairs with the task list at this helper's boundary,
	// exactly as the CLI pairs it into task records before the request.
	if writes != nil {
		for i := range req.Tasks {
			req.Tasks[i].Writes = writes[i]
		}
	}
	result, err := New(worker, WithGitInspector(NewDefaultGitInspector())).Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return result
}

// declaredPaths builds a task's write declaration with Declared set;
// called with no paths it is the declaration that the task writes
// nothing.
func declaredPaths(paths ...string) WriteDeclaration {
	return WriteDeclaration{Declared: true, Paths: paths}
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
	}}, dir, []string{"a"}, []WriteDeclaration{declaredPaths("file.txt")})
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

func TestControllerWritesFromSubdirectoryReanchorDeclarations(t *testing.T) {
	// --writes paths are relative to the run workspace, while the change
	// manifest is relative to the repository root. Both directions matter:
	// the workspace file must be accepted, and the root-level sibling with
	// the same relative spelling must remain undeclared.
	repo := newGitRepo(t)
	workspace := filepath.Join(repo, "sub")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	result := runWithWrites(t, &changesMutatingWorker{mutate: func(dir string) error {
		if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("workspace\n"), 0o644); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(repo, "file.txt"), []byte("root\n"), 0o644)
	}}, workspace, []string{"a"}, []WriteDeclaration{declaredPaths("file.txt")})
	writes := result.Writes
	if writes == nil || writes.Skipped != "" {
		t.Fatalf("writes = %#v, want a verdict", writes)
	}
	if writes.UndeclaredCount != 1 || len(writes.Undeclared) != 1 || writes.Undeclared[0] != "file.txt" || writes.Truncated {
		t.Fatalf("writes = %#v, want only root-level file.txt undeclared", writes)
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
	}}, dir, []string{"a"}, []WriteDeclaration{declaredPaths("src/a.txt")})
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
	}}, dir, []string{"a"}, []WriteDeclaration{declaredPaths("src/a")})
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
	}}, dir, []string{"a"}, []WriteDeclaration{declaredPaths("src/a")})
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

func TestControllerWritesDeclaredEmptySetCheckedCleanWhenNothingChanged(t *testing.T) {
	// A task that declared it writes nothing, on a run that changed
	// nothing, gets a clean verdict, not a skip: the read-only run is
	// proven read-only, which is the whole point of the writes-nothing
	// declaration.
	dir := newGitRepo(t)
	result := runWithWrites(t, &changesMutatingWorker{mutate: func(dir string) error {
		return nil
	}}, dir, []string{"a"}, []WriteDeclaration{declaredPaths()})
	writes := result.Writes
	if writes == nil {
		t.Fatalf("writes = nil, want a verdict on a declared writes-nothing run")
	}
	if writes.Skipped != "" || writes.UndeclaredCount != 0 || len(writes.Undeclared) != 0 || writes.Truncated {
		t.Fatalf("writes = %#v, want checked-clean", writes)
	}
}

func TestControllerWritesDeclaredEmptySetReportsStrayPathUndeclared(t *testing.T) {
	// A task that declared it writes nothing, on a run that changed one
	// path: that path is undeclared. The empty set is a real declaration
	// the check holds the run to, not an absence the check skips over.
	dir := newGitRepo(t)
	result := runWithWrites(t, &changesMutatingWorker{mutate: func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "file.txt"), []byte("changed\n"), 0o644)
	}}, dir, []string{"a"}, []WriteDeclaration{declaredPaths()})
	writes := result.Writes
	if writes == nil || writes.Skipped != "" {
		t.Fatalf("writes = %#v, want a verdict", writes)
	}
	if writes.UndeclaredCount != 1 || len(writes.Undeclared) != 1 || writes.Undeclared[0] != "file.txt" || writes.Truncated {
		t.Fatalf("writes = %#v, want exactly file.txt undeclared", writes)
	}
}

func TestControllerWritesOneTaskDeclaresOneDeclaresWritesNothing(t *testing.T) {
	// A run where one task declares paths and another declares the empty
	// set: both declared, so the check runs, and only paths outside the
	// pooled declaration are undeclared. The writes-nothing task's
	// emptiness is a statement, not a gap in the declaration.
	dir := newGitRepo(t)
	result := runWithWrites(t, &changesMutatingWorker{mutate: func(dir string) error {
		if err := os.MkdirAll(filepath.Join(dir, "src", "a"), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "src", "a", "a.txt"), []byte("a\n"), 0o644); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("stray\n"), 0o644)
	}}, dir, []string{"a", "b"}, []WriteDeclaration{declaredPaths("src/a"), declaredPaths()})
	writes := result.Writes
	if writes == nil || writes.Skipped != "" {
		t.Fatalf("writes = %#v, want a verdict: the writes-nothing task declared, so the check runs", writes)
	}
	if writes.UndeclaredCount != 1 || len(writes.Undeclared) != 1 || writes.Undeclared[0] != "stray.txt" || writes.Truncated {
		t.Fatalf("writes = %#v, want exactly stray.txt undeclared", writes)
	}
}

func TestControllerRejectsPartialWriteDeclaration(t *testing.T) {
	// A partial write declaration is a usage error, not a state worth
	// running: the request is rejected before any worker starts, and
	// the error names the first task that declared nothing. The
	// writes-nothing declaration is how a task that will not write
	// takes part, so no expressible intent is lost to the rejection.
	dir := newGitRepo(t)
	req := validRequest("a", "b")
	req.Workspace = dir
	req.Tasks[0].Writes = declaredPaths("file.txt")
	_, err := New(&changesMutatingWorker{mutate: func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "file.txt"), []byte("changed\n"), 0o644)
	}}, WithGitInspector(NewDefaultGitInspector())).Run(context.Background(), req)
	const want = "task 2 declared no writes while another task declared: the declaration is all-or-none; declare this task's paths, or declare the empty set if it writes nothing"
	if err == nil || err.Error() != want {
		t.Fatalf("run error = %v, want %q", err, want)
	}
	// No worker ran: file.txt still holds the four bytes newGitRepo
	// committed. That is independent evidence — a worker that ran
	// would have replaced them with "changed\n".
	got, err := os.ReadFile(filepath.Join(dir, "file.txt"))
	if err != nil {
		t.Fatalf("read file.txt: %v", err)
	}
	if string(got) != "one\n" {
		t.Fatalf("file.txt = %q, want %q: a worker must not have run", got, "one\n")
	}
}

func TestControllerWritesVerdictOnDirtyBeforeState(t *testing.T) {
	// A dirty before-state is measured, so the write check answers: the
	// untouched dirty path is subtracted from the manifest, nothing was
	// changed, and the declared path comes back clean — a verdict, not
	// a manifest-unavailable skip.
	dir := newGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	result := runWithWrites(t, newScriptedWorker(), dir, []string{"a"}, []WriteDeclaration{declaredPaths("file.txt")})
	if result.Changes == nil || result.Changes.Omitted != "" {
		t.Fatalf("changes = %#v, want a measured manifest on the dirty before-state", result.Changes)
	}
	if result.Changes.TotalFiles != 0 {
		t.Fatalf("changes = %#v, want zero changed paths", result.Changes)
	}
	writes := result.Writes
	if writes == nil {
		t.Fatalf("writes = nil, want a verdict")
	}
	if writes.Skipped != "" {
		t.Fatalf("skipped = %q, want a verdict", writes.Skipped)
	}
	if writes.UndeclaredCount != 0 || len(writes.Undeclared) != 0 || writes.Truncated {
		t.Fatalf("writes = %#v, want checked-clean", writes)
	}
	document := writeCheckDocument(t, writes)
	assertExactJSONKeys(t, document, "undeclaredCount")
	if document["undeclaredCount"] != float64(0) {
		t.Fatalf("undeclaredCount = %v, want present 0", document["undeclaredCount"])
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
	}}, dir, []string{"a"}, []WriteDeclaration{declaredPaths(declared...)})
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

func TestControllerWritesSpellingsCleanThroughFullPipeline(t *testing.T) {
	// The accepted spellings were only ever driven by calling checkWrites
	// directly on a hand-built Changes; the trailing slash and the
	// doubled separator must also travel the real route — validate, a
	// real git workspace, the manifest, and then the check — with a path
	// changed beneath the declaration coming back clean.
	tests := []string{
		"internal/run/", // trailing slash
		"internal//run", // doubled separator
	}
	for _, declared := range tests {
		t.Run(declared, func(t *testing.T) {
			dir := newGitRepo(t)
			result := runWithWrites(t, &changesMutatingWorker{mutate: func(dir string) error {
				if err := os.MkdirAll(filepath.Join(dir, "internal", "run"), 0o755); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(dir, "internal", "run", "x.go"), []byte("package run\n"), 0o644)
			}}, dir, []string{"a"}, []WriteDeclaration{declaredPaths(declared)})
			writes := result.Writes
			if writes == nil || writes.Skipped != "" {
				t.Fatalf("writes = %#v, want a verdict", writes)
			}
			if writes.UndeclaredCount != 0 || len(writes.Undeclared) != 0 || writes.Truncated {
				t.Fatalf("writes = %#v, want checked-clean for declared %q", writes, declared)
			}
		})
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
	}}, dir, []string{"a"}, []WriteDeclaration{declaredPaths(declared...)})
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
	req.Tasks[0].Writes = declaredPaths("declared.txt")
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
			check := checkWrites(&Changes{allPaths: []string{"internal/run/x.go"}}, []Task{{Writes: declaredPaths(declared)}}, "")
			if check.Skipped != "" || check.UndeclaredCount != 0 || len(check.Undeclared) != 0 || check.Truncated {
				t.Fatalf("writes = %#v, want checked-clean for declared %q", check, declared)
			}
		})
	}
}

func TestWriteCheckUndeclaredListFullAtCapNotTruncated(t *testing.T) {
	// Exactly maxChangeFiles undeclared paths: the list is full, the
	// cap dropped nothing, and Truncated must not be set. This is the
	// boundary itself — an off-by-one in either direction would pass
	// the driven counts alone — and UndeclaredCount must carry the true
	// count, not the length of the list, which happens to agree here
	// only because nothing was dropped.
	paths := make([]string, maxChangeFiles)
	for i := range paths {
		paths[i] = fmt.Sprintf("f%03d.txt", i)
	}
	check := checkWrites(&Changes{allPaths: paths}, []Task{{Writes: declaredPaths("declared.txt")}}, "")
	if check.Skipped != "" {
		t.Fatalf("writes = %#v, want a verdict", check)
	}
	if check.Truncated {
		t.Fatalf("truncated = true, want false when the cap dropped nothing")
	}
	if check.UndeclaredCount != maxChangeFiles || len(check.Undeclared) != maxChangeFiles {
		t.Fatalf("undeclaredCount = %d, undeclared = %d entries, want both %d", check.UndeclaredCount, len(check.Undeclared), maxChangeFiles)
	}
}

func TestWriteCheckUndeclaredListCappedOnePastCap(t *testing.T) {
	// One more than maxChangeFiles undeclared paths: the cap drops the
	// last entry and sets Truncated. UndeclaredCount must still carry
	// the true count, not the length of the capped list.
	paths := make([]string, maxChangeFiles+1)
	for i := range paths {
		paths[i] = fmt.Sprintf("f%03d.txt", i)
	}
	check := checkWrites(&Changes{allPaths: paths}, []Task{{Writes: declaredPaths("declared.txt")}}, "")
	if check.Skipped != "" {
		t.Fatalf("writes = %#v, want a verdict", check)
	}
	if !check.Truncated {
		t.Fatalf("truncated = false, want true when the cap dropped an entry")
	}
	if check.UndeclaredCount != maxChangeFiles+1 {
		t.Fatalf("undeclaredCount = %d, want %d", check.UndeclaredCount, maxChangeFiles+1)
	}
	if len(check.Undeclared) != maxChangeFiles {
		t.Fatalf("undeclared = %d entries, want the %d cap", len(check.Undeclared), maxChangeFiles)
	}
	// The paths sort by path, so the cap keeps the first hundred.
	if check.Undeclared[maxChangeFiles-1] != "f099.txt" {
		t.Fatalf("last undeclared = %q, want f099.txt", check.Undeclared[maxChangeFiles-1])
	}
}
func TestWriteCheckNilManifestSkips(t *testing.T) {
	// A nil *Changes only reaches the check when the run carried no git
	// inspector at all, which every controller-driven run with one does
	// through an Omitted reason instead — including the
	// work-tree-unconfirmed omission, now that the controller states it
	// rather than leaving the field absent. The skip reason must be the
	// same either way.
	check := checkWrites(nil, []Task{{Writes: declaredPaths("file.txt")}}, "")
	if check.Skipped != reasonManifestUnavailable {
		t.Fatalf("skipped = %q, want %q", check.Skipped, reasonManifestUnavailable)
	}
	if check.UndeclaredCount != 0 || check.Undeclared != nil || check.Truncated {
		t.Fatalf("writes = %#v, want no fields alongside the skip reason", check)
	}
}

func TestControllerWritesSkipWorktreeTrackedRewriteUnavailable(t *testing.T) {
	// Regression for #78: setting skip-worktree before a tracked rewrite
	// makes the post-run index-backed Git diff hide the file. The controller
	// must not report a measured-zero manifest and checked-clean write verdict
	// for a file the worker actually rewrote and did not declare; metadata
	// drift makes the measurement unavailable instead.
	dir := newGitRepo(t)
	result := runWithWrites(t, &changesMutatingWorker{mutate: func(dir string) error {
		cmd := exec.Command("git", "update-index", "--skip-worktree", "--", "file.txt")
		cmd.Dir = dir
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("set skip-worktree: %v\\n%s", err, output)
		}
		return os.WriteFile(filepath.Join(dir, "file.txt"), []byte("rewritten\n"), 0o644)
	}}, dir, []string{"a"}, []WriteDeclaration{declaredPaths("declared.txt")})
	if got, err := os.ReadFile(filepath.Join(dir, "file.txt")); err != nil || string(got) != "rewritten\n" {
		t.Fatalf("tracked file after run = %q, err %v; want the same-run rewrite", got, err)
	}
	changes := result.Changes
	if changes == nil || changes.Omitted != reasonMeasurementFail {
		t.Fatalf("changes = %#v, want the exact measurement-failed omission", changes)
	}
	if changes.Files != nil || changes.TotalFiles != 0 || changes.Truncated {
		t.Fatalf("changes = %#v, want no measured fields alongside the omission", changes)
	}
	writes := result.Writes
	if writes == nil || writes.Skipped != reasonManifestUnavailable {
		t.Fatalf("writes = %#v, want the exact unavailable skip", writes)
	}
	if writes.UndeclaredCount != 0 || writes.Undeclared != nil || writes.Truncated {
		t.Fatalf("writes = %#v, want no verdict fields alongside the skip", writes)
	}
}

func TestControllerWritesSameRunExcludeHidesUntrackedFileUnavailable(t *testing.T) {
	// Regression for #78: changing .git/info/exclude before creating an
	// untracked file makes the post-run exclude-standard listing hide it. The
	// controller must not report measured-zero and checked-clean for that
	// undeclared same-run file; metadata drift makes the measurement
	// unavailable instead.
	dir := newGitRepo(t)
	result := runWithWrites(t, &changesMutatingWorker{mutate: func(dir string) error {
		exclude := filepath.Join(dir, ".git", "info", "exclude")
		file, err := os.OpenFile(exclude, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if _, err := file.WriteString("\nhidden.txt\n"); err != nil {
			file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "hidden.txt"), []byte("hidden\n"), 0o644)
	}}, dir, []string{"a"}, []WriteDeclaration{declaredPaths("declared.txt")})
	if _, err := os.Stat(filepath.Join(dir, "hidden.txt")); err != nil {
		t.Fatalf("untracked file after run: %v", err)
	}
	changes := result.Changes
	if changes == nil || changes.Omitted != reasonMeasurementFail {
		t.Fatalf("changes = %#v, want the exact measurement-failed omission", changes)
	}
	if changes.Files != nil || changes.TotalFiles != 0 || changes.Truncated {
		t.Fatalf("changes = %#v, want no measured fields alongside the omission", changes)
	}
	writes := result.Writes
	if writes == nil || writes.Skipped != reasonManifestUnavailable {
		t.Fatalf("writes = %#v, want the exact unavailable skip", writes)
	}
	if writes.UndeclaredCount != 0 || writes.Undeclared != nil || writes.Truncated {
		t.Fatalf("writes = %#v, want no verdict fields alongside the skip", writes)
	}
}

func TestControllerWritesDirtyBeforeSameSizeRewriteRestoredMtimeUndeclared(t *testing.T) {
	// Regression for #71, write-check side: an already-dirty tracked
	// file that the worker rewrites with different content of the same
	// size and a restored modification time used to drop out of the
	// manifest entirely — its stat stamp matched, so it was subtracted —
	// and the write check then reported checked-clean for a write that
	// was never declared. The content-identity stamp keeps the path in
	// the manifest, and the check must answer with an undeclared
	// verdict, never a skip and never silence.
	dir := newGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	info, err := os.Lstat(filepath.Join(dir, "file.txt"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	stampTime := info.ModTime()
	result := runWithWrites(t, &changesMutatingWorker{mutate: func(dir string) error {
		if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("clean\n"), 0o644); err != nil {
			return err
		}
		return os.Chtimes(filepath.Join(dir, "file.txt"), stampTime, stampTime)
	}}, dir, []string{"a"}, []WriteDeclaration{declaredPaths("declared.txt")})
	changes := result.Changes
	if changes == nil || changes.Omitted != "" {
		t.Fatalf("changes = %#v, want a measured manifest", changes)
	}
	if changes.TotalFiles != 1 || len(changes.Files) != 1 {
		t.Fatalf("changes = %#v, want the rewritten path in the manifest", changes)
	}
	writes := result.Writes
	if writes == nil || writes.Skipped != "" {
		t.Fatalf("writes = %#v, want a verdict, not a skip", writes)
	}
	if writes.UndeclaredCount != 1 || len(writes.Undeclared) != 1 || writes.Undeclared[0] != "file.txt" || writes.Truncated {
		t.Fatalf("writes = %#v, want file.txt the single undeclared path", writes)
	}
}

func TestControllerWritesPreExistingSkipWorktreeUnavailable(t *testing.T) {
	// Regression for #78, declared-writes side of a pre-existing marker:
	// a skip-worktree entry already set when the run started hides any
	// rewrite of its file from the measurement with nothing for the
	// post-run drift check to catch. Even a run that changed nothing
	// must not report checked-clean under an index that cannot support
	// the claim: the manifest omits with measurement-failed and the
	// declared-writes check skips with the manifest-unavailable reason.
	dir := newGitRepo(t)
	runGit(t, dir, "update-index", "--skip-worktree", "--", "file.txt")
	result := runWithWrites(t, newScriptedWorker(), dir, []string{"a"}, []WriteDeclaration{declaredPaths("declared.txt")})
	changes := result.Changes
	if changes == nil || changes.Omitted != reasonMeasurementFail {
		t.Fatalf("changes = %#v, want the exact measurement-failed omission", changes)
	}
	if changes.Files != nil || changes.TotalFiles != 0 || changes.Truncated {
		t.Fatalf("changes = %#v, want no measured fields alongside the omission", changes)
	}
	writes := result.Writes
	if writes == nil || writes.Skipped != reasonManifestUnavailable {
		t.Fatalf("writes = %#v, want the exact unavailable skip", writes)
	}
	if writes.UndeclaredCount != 0 || writes.Undeclared != nil || writes.Truncated {
		t.Fatalf("writes = %#v, want no verdict fields alongside the skip", writes)
	}
}

func TestControllerWritesPreExistingTrustCtimeFalseUnavailable(t *testing.T) {
	// Regression for #130, declared-writes side: core.trustctime=false
	// already set before the run lets git trust the stat cache over
	// content, so the post-run measurement cannot see a same-size
	// rewrite with a restored modification time at all — git itself
	// reports nothing. A run that changed nothing would otherwise
	// produce a confident zero and a checked-clean verdict the
	// repository's own trust setting cannot support; the manifest
	// omits with measurement-failed and the declared-writes check
	// skips.
	dir := newGitRepo(t)
	runGit(t, dir, "config", "core.trustctime", "false")
	result := runWithWrites(t, newScriptedWorker(), dir, []string{"a"}, []WriteDeclaration{declaredPaths("declared.txt")})
	changes := result.Changes
	if changes == nil || changes.Omitted != reasonMeasurementFail {
		t.Fatalf("changes = %#v, want the exact measurement-failed omission", changes)
	}
	if changes.Files != nil || changes.TotalFiles != 0 || changes.Truncated {
		t.Fatalf("changes = %#v, want no measured fields alongside the omission", changes)
	}
	writes := result.Writes
	if writes == nil || writes.Skipped != reasonManifestUnavailable {
		t.Fatalf("writes = %#v, want the exact unavailable skip", writes)
	}
	if writes.UndeclaredCount != 0 || writes.Undeclared != nil || writes.Truncated {
		t.Fatalf("writes = %#v, want no verdict fields alongside the skip", writes)
	}
}

func TestControllerWritesSameRunExcludesFileValueRedirectUnavailable(t *testing.T) {
	// Regression for #78, core.excludesFile side: the exclude-standard
	// listing honours the effective core.excludesFile, so redirecting
	// that value during the run can hide untracked paths the run wrote
	// behind rules the pre-run snapshot never saw. The effective value
	// is a recorded trust input, and a value that moved makes the
	// measurement unavailable rather than a confident clean.
	dir := newGitRepo(t)
	first := filepath.Join(dir, ".git", "excludes-a")
	second := filepath.Join(dir, ".git", "excludes-b")
	if err := os.WriteFile(first, []byte("hidden.txt\n"), 0o644); err != nil {
		t.Fatalf("write excludes-a: %v", err)
	}
	if err := os.WriteFile(second, []byte("hidden.txt\n"), 0o644); err != nil {
		t.Fatalf("write excludes-b: %v", err)
	}
	runGit(t, dir, "config", "core.excludesFile", first)
	result := runWithWrites(t, &changesMutatingWorker{mutate: func(dir string) error {
		cmd := exec.Command("git", "config", "core.excludesFile", second)
		cmd.Dir = dir
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("redirect excludesFile: %v\n%s", err, output)
		}
		return os.WriteFile(filepath.Join(dir, "hidden.txt"), []byte("hidden\n"), 0o644)
	}}, dir, []string{"a"}, []WriteDeclaration{declaredPaths("declared.txt")})
	if _, err := os.Stat(filepath.Join(dir, "hidden.txt")); err != nil {
		t.Fatalf("untracked file after run: %v", err)
	}
	changes := result.Changes
	if changes == nil || changes.Omitted != reasonMeasurementFail {
		t.Fatalf("changes = %#v, want the exact measurement-failed omission", changes)
	}
	writes := result.Writes
	if writes == nil || writes.Skipped != reasonManifestUnavailable {
		t.Fatalf("writes = %#v, want the exact unavailable skip", writes)
	}
	if writes.UndeclaredCount != 0 || writes.Undeclared != nil || writes.Truncated {
		t.Fatalf("writes = %#v, want no verdict fields alongside the skip", writes)
	}
}

func TestControllerWritesSameRunExcludesFileTargetModifiedUnavailable(t *testing.T) {
	// Regression for #78, core.excludesFile file side: a worker that
	// appends a rule to the file the effective core.excludesFile names
	// changes the ignore-rule input in place — the value is untouched,
	// so only the file's stamp can see the move. The stamped file is a
	// recorded trust input, and drift makes the measurement unavailable
	// rather than a confident clean.
	dir := newGitRepo(t)
	excludes := filepath.Join(dir, ".git", "test-excludes")
	if err := os.WriteFile(excludes, []byte("# empty\n"), 0o644); err != nil {
		t.Fatalf("write excludes: %v", err)
	}
	runGit(t, dir, "config", "core.excludesFile", excludes)
	result := runWithWrites(t, &changesMutatingWorker{mutate: func(dir string) error {
		file, err := os.OpenFile(excludes, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if _, err := file.WriteString("hidden.txt\n"); err != nil {
			file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "hidden.txt"), []byte("hidden\n"), 0o644)
	}}, dir, []string{"a"}, []WriteDeclaration{declaredPaths("declared.txt")})
	if _, err := os.Stat(filepath.Join(dir, "hidden.txt")); err != nil {
		t.Fatalf("untracked file after run: %v", err)
	}
	changes := result.Changes
	if changes == nil || changes.Omitted != reasonMeasurementFail {
		t.Fatalf("changes = %#v, want the exact measurement-failed omission", changes)
	}
	writes := result.Writes
	if writes == nil || writes.Skipped != reasonManifestUnavailable {
		t.Fatalf("writes = %#v, want the exact unavailable skip", writes)
	}
	if writes.UndeclaredCount != 0 || writes.Undeclared != nil || writes.Truncated {
		t.Fatalf("writes = %#v, want no verdict fields alongside the skip", writes)
	}
}

func TestControllerWritesIgnoredUntrackedPathOutsideManifest(t *testing.T) {
	// The untracked pass (git ls-files --others --exclude-standard)
	// honours ignore rules, so an ignored path a worker wrote is outside
	// the manifest: it appears in no file entry and counts toward
	// nothing, and the write check — which compares against the
	// manifest — cannot see it either. A run that wrote only such a path
	// reports a clean verdict even though the path was never declared:
	// the documented false-clean, pinned so a future change to the
	// untracked pass cannot make it silently different.
	dir := newGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	gitCommit(t, dir)
	result := runWithWrites(t, &changesMutatingWorker{mutate: func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("hidden\n"), 0o644)
	}}, dir, []string{"a"}, []WriteDeclaration{declaredPaths("declared.txt")})
	changes := result.Changes
	if changes == nil || changes.Omitted != "" {
		t.Fatalf("changes = %#v, want a measured manifest", changes)
	}
	if changes.TotalFiles != 0 || len(changes.Files) != 0 || changes.Truncated {
		t.Fatalf("changes = %#v, want measured-zero: the ignored path is outside the manifest", changes)
	}
	writes := result.Writes
	if writes == nil || writes.Skipped != "" {
		t.Fatalf("writes = %#v, want a verdict", writes)
	}
	if writes.UndeclaredCount != 0 || len(writes.Undeclared) != 0 || writes.Truncated {
		t.Fatalf("writes = %#v, want checked-clean: the ignored path was never declared and never seen", writes)
	}
}

func TestControllerWritesTrackedPathMatchedByIgnoreRuleStillChecked(t *testing.T) {
	// Ignore rules do not apply to files git already tracks: the tracked
	// pass (git diff against the before-state HEAD) lists every tracked
	// change regardless of a matching rule, so a tracked path a rule
	// matches is still measured and still checked. Modified and
	// undeclared, it appears in the manifest and counts as undeclared.
	dir := newGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("file.txt\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	gitCommit(t, dir)
	result := runWithWrites(t, &changesMutatingWorker{mutate: func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "file.txt"), []byte("two\n"), 0o644)
	}}, dir, []string{"a"}, []WriteDeclaration{declaredPaths("declared.txt")})
	changes := result.Changes
	if changes == nil || changes.Omitted != "" {
		t.Fatalf("changes = %#v, want a measured manifest", changes)
	}
	var found bool
	for _, file := range changes.Files {
		if file.Path == "file.txt" {
			found = true
			if file.Status != "modified" || file.Added != 1 || file.Deleted != 1 {
				t.Fatalf("file = %#v, want file.txt modified +1/-1", file)
			}
		}
	}
	if !found {
		t.Fatalf("file.txt missing from the manifest despite being tracked and matched by an ignore rule")
	}
	if changes.TotalFiles != 1 {
		t.Fatalf("totalFiles = %d, want 1", changes.TotalFiles)
	}
	writes := result.Writes
	if writes == nil || writes.Skipped != "" {
		t.Fatalf("writes = %#v, want a verdict", writes)
	}
	if writes.UndeclaredCount != 1 || len(writes.Undeclared) != 1 || writes.Undeclared[0] != "file.txt" || writes.Truncated {
		t.Fatalf("writes = %#v, want exactly file.txt undeclared", writes)
	}
}

func TestControllerWritesDeletedTrackedFileUndeclared(t *testing.T) {
	// Deleting a file is writing to the workspace: a run that removed a
	// tracked file it never declared must be told, with the deletion
	// recorded in the manifest and the path reported undeclared.
	dir := newGitRepo(t)
	result := runWithWrites(t, &changesMutatingWorker{mutate: func(dir string) error {
		return os.Remove(filepath.Join(dir, "file.txt"))
	}}, dir, []string{"a"}, []WriteDeclaration{declaredPaths("declared.txt")})
	changes := result.Changes
	if changes == nil || changes.Omitted != "" || len(changes.Files) != 1 || changes.Files[0].Status != "deleted" {
		t.Fatalf("changes = %#v, want file.txt deleted in the manifest", changes)
	}
	writes := result.Writes
	if writes == nil || writes.Skipped != "" {
		t.Fatalf("writes = %#v, want a verdict", writes)
	}
	if writes.UndeclaredCount != 1 || len(writes.Undeclared) != 1 || writes.Undeclared[0] != "file.txt" || writes.Truncated {
		t.Fatalf("writes = %#v, want exactly file.txt undeclared", writes)
	}
}

func TestControllerWritesDeletedTrackedFileDeclaredClean(t *testing.T) {
	// A caller who declared the deleted file gets a clean verdict: the
	// manifest gives a deletion the deleted status and checkWrites
	// compares its path like any other.
	dir := newGitRepo(t)
	result := runWithWrites(t, &changesMutatingWorker{mutate: func(dir string) error {
		return os.Remove(filepath.Join(dir, "file.txt"))
	}}, dir, []string{"a"}, []WriteDeclaration{declaredPaths("file.txt")})
	writes := result.Writes
	if writes == nil || writes.Skipped != "" {
		t.Fatalf("writes = %#v, want a verdict", writes)
	}
	if writes.UndeclaredCount != 0 || len(writes.Undeclared) != 0 || writes.Truncated {
		t.Fatalf("writes = %#v, want checked-clean for the declared deletion", writes)
	}
}

func TestControllerWritesTwoTasksPoolDeclaredPathsClean(t *testing.T) {
	// checkWrites pools every task's declared paths into one set: two
	// tasks declaring disjoint paths, each writing beneath its own
	// declaration, come back clean because the pooled set covers every
	// changed path.
	dir := newGitRepo(t)
	result := runWithWrites(t, &changesMutatingWorker{mutate: func(dir string) error {
		if err := os.MkdirAll(filepath.Join(dir, "src", "a"), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "src", "a", "a.txt"), []byte("a\n"), 0o644); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(dir, "src", "b"), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "src", "b", "b.txt"), []byte("b\n"), 0o644)
	}}, dir, []string{"a", "b"}, []WriteDeclaration{declaredPaths("src/a"), declaredPaths("src/b")})
	writes := result.Writes
	if writes == nil || writes.Skipped != "" {
		t.Fatalf("writes = %#v, want a verdict", writes)
	}
	if writes.UndeclaredCount != 0 || len(writes.Undeclared) != 0 || writes.Truncated {
		t.Fatalf("writes = %#v, want checked-clean: both declarations pooled cover every changed path", writes)
	}
}

func TestControllerWritesTwoTasksPoolReportsStrayPathOnce(t *testing.T) {
	// A path outside both declarations is reported once, not once per
	// task: the undeclared set belongs to the run, and pooling is the
	// code that makes that true.
	dir := newGitRepo(t)
	result := runWithWrites(t, &changesMutatingWorker{mutate: func(dir string) error {
		if err := os.MkdirAll(filepath.Join(dir, "src", "a"), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "src", "a", "a.txt"), []byte("a\n"), 0o644); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(dir, "src", "b"), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "src", "b", "b.txt"), []byte("b\n"), 0o644); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "src", "stray.txt"), []byte("stray\n"), 0o644)
	}}, dir, []string{"a", "b"}, []WriteDeclaration{declaredPaths("src/a"), declaredPaths("src/b")})
	writes := result.Writes
	if writes == nil || writes.Skipped != "" {
		t.Fatalf("writes = %#v, want a verdict", writes)
	}
	if writes.UndeclaredCount != 1 || len(writes.Undeclared) != 1 || writes.Undeclared[0] != "src/stray.txt" || writes.Truncated {
		t.Fatalf("writes = %#v, want src/stray.txt reported exactly once", writes)
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

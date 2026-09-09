//go:build livepi

// Package livepiprobe live probes: real Pi, real model, never in CI. Each
// scenario proves that Pi's own built-in tools resolve relative paths inside
// the workspace pi-worker selected, not the caller's inherited working
// directory.
package livepiprobe

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arasovic/pi-worker/internal/testutil/livepi"
)

// probeTimeout is the generous run budget: a live model answering a
// multi-tool prompt can take minutes.
const probeTimeout = 15 * time.Minute

// probePrompt is the same prompt for both scenarios. It asks the model to use
// relative paths only, so any tool call resolves against whatever directory Pi
// treats as its working directory — which is exactly what is under test. It
// demands verbatim reporting of every path and content line seen, and forbids
// edits, writes, and shell commands.
const probePrompt = `You are being probed. Using RELATIVE paths only, and only the read, ` +
	`list, find, and grep tools — never edit, write, or run shell commands — do the following: ` +
	`1) read the file probe/marker.txt; ` +
	`2) list the contents of the probe directory; ` +
	`3) find or grep for files whose name begins with "found-". ` +
	`Then report, verbatim, every file path you saw and every line of file content you saw. ` +
	`Report exactly what the tools returned; do not summarize, paraphrase, or invent anything.`

// buildState holds the once-per-test-run build of the real pi-worker binary,
// mirroring the fakepi helper idiom in internal/background.
var (
	buildOnce  sync.Once
	binPath    string
	binDir     string
	buildError error
)

// buildPiWorkerOnce builds ./cmd/pi-worker into a temporary directory once
// per test run.
func buildPiWorkerOnce() {
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "pi-worker-livepiprobe-bin-*")
		if err != nil {
			buildError = err
			return
		}
		binDir = dir
		binPath = filepath.Join(dir, "pi-worker")
		build := exec.Command("go", "build", "-o", binPath, "github.com/arasovic/pi-worker/cmd/pi-worker")
		if out, err := build.CombinedOutput(); err != nil {
			buildError = err
			binPath = ""
			_ = os.RemoveAll(dir)
			binDir = ""
			if len(out) > 0 {
				buildError = &outputError{err: err, output: string(out)}
			}
		}
	})
}

type outputError struct {
	err    error
	output string
}

func (e *outputError) Error() string { return e.err.Error() + "\n" + e.output }

// piWorkerBin returns the built pi-worker binary path, building it on first
// use.
func piWorkerBin(t *testing.T) string {
	t.Helper()
	buildPiWorkerOnce()
	if buildError != nil {
		t.Fatalf("build pi-worker: %v", buildError)
	}
	// The build is shared by every probe in this run, so it is removed once
	// in TestMain rather than by the first test that happens to finish.
	return binPath
}

// TestMain removes the shared build directory after every probe has run.
func TestMain(m *testing.M) {
	code := m.Run()
	if binDir != "" {
		_ = os.RemoveAll(binDir)
	}
	os.Exit(code)
}

// nonce returns 16 random hex characters from crypto/rand. The nonce is the
// whole proof: a model cannot invent 16 random hex characters, so finding the
// selected nonce in the reported text is real evidence that a tool actually
// read the selected workspace.
func nonce(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("generate nonce: %v", err)
	}
	return hex.EncodeToString(raw)
}

// writeFixture writes the probe fixture files for one nonce into dir:
// probe/marker.txt and probe/found-<nonce>.txt, each containing the nonce.
func writeFixture(t *testing.T, dir, value string) {
	t.Helper()
	probeDir := filepath.Join(dir, "probe")
	if err := os.MkdirAll(probeDir, 0o755); err != nil {
		t.Fatalf("mkdir probe: %v", err)
	}
	for _, name := range []string{"marker.txt", "found-" + value + ".txt"} {
		if err := os.WriteFile(filepath.Join(probeDir, name), []byte(value+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// git helper: runs git in dir, failing the test on error.
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+t.TempDir())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// initRepo git-inits dir, configures a local identity, and makes one commit
// so the product sees a confirmed git work tree.
func initRepo(t *testing.T, dir string) {
	t.Helper()
	git(t, dir, "init", "-q")
	git(t, dir, "config", "user.email", "probe@pi-worker")
	git(t, dir, "config", "user.name", "livepiprobe")
	seed := filepath.Join(dir, "seed.txt")
	if err := os.WriteFile(seed, []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	git(t, dir, "add", "seed.txt")
	git(t, dir, "commit", "-q", "-m", "initial")
}

// runDocument is the projection of the pi-worker --json result document the
// assertions read.
type runDocument struct {
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
	Workers []struct {
		Status             string `json:"status"`
		Error              string `json:"error,omitempty"`
		Explanation        string `json:"explanation"`
		PartialExplanation string `json:"partialExplanation"`
	} `json:"workers"`
}

// runProbe runs the built binary in the current working directory — the
// caller must t.Chdir first — with the gate's model, the lowest thinking
// level, --json, and a generous timeout, then parses the single JSON document
// from stdout. No --verify, no --worktree, no --writes: this probe tests path
// resolution, nothing else.
func runProbe(t *testing.T, bin, model string, extra ...string) runDocument {
	t.Helper()
	args := append([]string{"run"}, extra...)
	cmd := exec.Command(bin, append(args,
		"--model", model,
		"--thinking", "off",
		"--timeout", probeTimeout.String(),
		"--json",
		"--task", probePrompt,
	)...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run pi-worker: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	var doc runDocument
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &doc); err != nil {
		t.Fatalf("parse run document: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	return doc
}

// assertProbe checks the shared assertions on the parsed document: the run
// completed, the reported text contains the selected nonce, and it never
// contains the decoy nonce. explanation is used; partialExplanation only
// when explanation is empty.
//
// Known residual gap: text-level proof. A leak is missed only if the model
// read the decoy and chose not to report it.
// ponytail: text-level proof; an unexported raw tool-result observer inside
// package pi would close the gap if a leak is ever suspected.
func assertProbe(t *testing.T, doc runDocument, selected, decoy string) {
	t.Helper()
	if doc.Status != "completed" {
		t.Fatalf("top-level status = %q, want completed (error=%q)", doc.Status, doc.Error)
	}
	if len(doc.Workers) != 1 {
		t.Fatalf("workers len = %d, want 1", len(doc.Workers))
	}
	worker := doc.Workers[0]
	if worker.Status != "completed" {
		t.Fatalf("worker status = %q, want completed (error=%q)", worker.Status, worker.Error)
	}
	text := worker.Explanation
	if text == "" {
		text = worker.PartialExplanation
	}
	if !strings.Contains(text, selected) {
		t.Fatalf("reported text does not contain the selected nonce %s\ntext:\n%s", selected, text)
	}
	if strings.Contains(text, decoy) {
		t.Fatalf("reported text contains the decoy nonce %s\ntext:\n%s", decoy, text)
	}
}

// TestProbeCallerCheckoutIsWorkspace proves scenario 1: with the caller's
// checkout as the workspace, Pi's tools resolve relative paths inside it. The
// selected fixture lives in the workspace; the decoy lives one level above in
// the parent directory. A tool that resolved relative paths above the
// selected workspace would see the decoy.
func TestProbeCallerCheckoutIsWorkspace(t *testing.T) {
	gate := livepi.Gate(t)
	bin := piWorkerBin(t)

	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	initRepo(t, workspace)

	selected := nonce(t)
	decoy := nonce(t)
	writeFixture(t, workspace, selected)
	// The decoy sits one level above the workspace: exactly where a
	// regressed tool resolving "probe/..." relative to the caller's
	// checkout parent, or any path escaping the workspace, would land.
	writeFixture(t, parent, decoy)

	t.Chdir(workspace)
	doc := runProbe(t, bin, gate.Model)
	assertProbe(t, doc, selected, decoy)
}

// TestProbeManagedWorktreeIsWorkspace proves scenario 2: with --worktree, the
// selected workspace is the managed worktree, not the repository root the
// caller sits in. The repository root holds the decoy nonce on the same
// relative paths while the freshly created worktree holds the selected one —
// exactly where a regressed Pi that ignored the selected workspace and used
// its inherited working directory would read.
//
// The decoy overwrites the committed selected fixture at the repository root
// as uncommitted changes. This is why the assertion holds: worktree.Prepare
// checks out HEAD into a new linked worktree, and a fresh checkout carries
// committed content only — the uncommitted working-tree changes in the
// repository root never reach it.
func TestProbeManagedWorktreeIsWorkspace(t *testing.T) {
	gate := livepi.Gate(t)
	bin := piWorkerBin(t)

	repo := t.TempDir()
	initRepo(t, repo)

	selected := nonce(t)
	decoy := nonce(t)
	// Commit the selected fixture so a worktree created from HEAD holds it.
	writeFixture(t, repo, selected)
	git(t, repo, "add", "probe")
	git(t, repo, "commit", "-q", "-m", "probe fixture")
	// Overwrite the same relative paths at the repository root as
	// uncommitted files: the root now holds the decoy while HEAD — and
	// therefore any fresh worktree — still holds the selected nonce.
	writeFixture(t, repo, decoy)
	marker, err := os.ReadFile(filepath.Join(repo, "probe", "marker.txt"))
	if err != nil {
		t.Fatalf("read root marker: %v", err)
	}
	if got := strings.TrimSpace(string(marker)); got != decoy {
		t.Fatalf("precondition: repository root marker = %q, want decoy %q", got, decoy)
	}

	name := "probe-" + nonce(t)
	t.Chdir(repo)
	doc := runProbe(t, bin, gate.Model, "--worktree", name)
	assertProbe(t, doc, selected, decoy)

	// Remove the managed worktree so the temporary repository can be
	// cleaned up: worktree remove refuses a linked checkout held by the
	// repository's worktree list.
	git(t, repo, "worktree", "remove", "--force", filepath.Join(repo, ".pi-worker", "worktrees", name))
	git(t, repo, "branch", "-D", "run/"+name)
}

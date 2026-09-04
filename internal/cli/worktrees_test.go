package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseWorktreesArgsAccepted asserts the accepted forms and the
// options they parse into: list with optional --json, and remove with
// exactly one valid name followed by optional --yes and --json in
// either order.
func TestParseWorktreesArgsAccepted(t *testing.T) {
	for _, test := range []struct {
		args []string
		want worktreesOptions
	}{
		{args: []string{"list"}, want: worktreesOptions{command: "list"}},
		{args: []string{"list", "--json"}, want: worktreesOptions{command: "list", json: true}},
		{args: []string{"remove", "alpha"}, want: worktreesOptions{command: "remove", name: "alpha"}},
		{args: []string{"remove", "alpha", "--yes"}, want: worktreesOptions{command: "remove", name: "alpha", yes: true}},
		{args: []string{"remove", "alpha", "--json"}, want: worktreesOptions{command: "remove", name: "alpha", json: true}},
		{args: []string{"remove", "alpha", "--yes", "--json"}, want: worktreesOptions{command: "remove", name: "alpha", yes: true, json: true}},
		{args: []string{"remove", "alpha", "--json", "--yes"}, want: worktreesOptions{command: "remove", name: "alpha", yes: true, json: true}},
	} {
		opts, err := parseWorktreesArgs(test.args)
		if err != nil {
			t.Fatalf("parseWorktreesArgs(%q): %v, want acceptance", test.args, err)
		}
		if opts != test.want {
			t.Fatalf("parseWorktreesArgs(%q) = %#v, want %#v", test.args, opts, test.want)
		}
	}
}

// TestParseWorktreesArgsRejected asserts every rejected form fails
// with a descriptive error: a missing subcommand, an unknown
// subcommand (including flag-before-subcommand), an unknown flag, an
// extra positional argument, a --json flag given a value, a repeated
// flag, an invalid or missing worktree name, a flag in place of the
// name, valued boolean flags, and remove-only --yes on list.
func TestParseWorktreesArgsRejected(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string // a substring every error must carry; empty means any error
	}{
		{args: []string{}, want: "worktrees requires a subcommand"},
		{args: []string{"list", "--json", "--json"}, want: "specified more than once"},
		{args: []string{"list", "--json=1"}, want: "does not take a value"},
		{args: []string{"list", "--bogus"}, want: "unknown flag"},
		{args: []string{"list", "--json", "--bogus"}, want: "unknown flag"},
		{args: []string{"list", "extra"}, want: "unexpected argument"},
		{args: []string{"list", "--json", "extra"}, want: "unexpected argument"},
		{args: []string{"list", "extra", "--json"}, want: "unexpected argument"},
		{args: []string{"list", "--yes"}, want: "not valid with"},
		{args: []string{"list", "--yes", "--json"}, want: "not valid with"},
		{args: []string{"prune"}, want: "unknown worktrees command"},
		{args: []string{"--json", "list"}, want: "unknown worktrees command"},
		{args: []string{"--json", "--json", "list"}, want: "unknown worktrees command"},
		{args: []string{"remove"}, want: "worktrees remove requires a name"},
		{args: []string{"remove", "--yes"}, want: "worktrees remove requires a name"},
		{args: []string{"remove", "--json"}, want: "worktrees remove requires a name"},
		{args: []string{"remove", "Bad"}, want: "invalid worktree name"},
		{args: []string{"remove", "alpha", "extra"}, want: "unexpected argument"},
		{args: []string{"remove", "alpha", "--yes", "extra"}, want: "unexpected argument"},
		{args: []string{"remove", "alpha", "--yes=1"}, want: "does not take a value"},
		{args: []string{"remove", "alpha", "--json=1"}, want: "does not take a value"},
		{args: []string{"remove", "alpha", "--yes", "--yes"}, want: "specified more than once"},
		{args: []string{"remove", "alpha", "--json", "--json"}, want: "specified more than once"},
		{args: []string{"remove", "alpha", "--bogus"}, want: "unknown flag"},
		{args: []string{"remove", "alpha", "--yes", "--bogus"}, want: "unknown flag"},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			_, err := parseWorktreesArgs(test.args)
			if err == nil {
				t.Fatalf("parseWorktreesArgs(%q) = nil error, want rejection", test.args)
			}
			if test.want != "" && !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parseWorktreesArgs(%q) error = %q, want it to contain %q", test.args, err, test.want)
			}
		})
	}
}

func TestWorktreesListHuman(t *testing.T) {
	repo := canonicalRepo(t, newGitWorkspace(t))
	bravoPath, _, err := prepareWorktree(context.Background(), repo, "bravo")
	if err != nil {
		t.Fatalf("prepareWorktree bravo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bravoPath, "file.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("dirty bravo: %v", err)
	}
	alphaPath, _, err := prepareWorktree(context.Background(), repo, "alpha")
	if err != nil {
		t.Fatalf("prepareWorktree alpha: %v", err)
	}
	if err := os.WriteFile(filepath.Join(alphaPath, "file.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatalf("write alpha: %v", err)
	}
	gitRun(t, alphaPath, "add", "file.txt")
	gitRun(t, alphaPath, "commit", "-q", "-m", "advance alpha")
	var stdout, stderr bytes.Buffer
	code := worktreesListCommand(context.Background(), worktreesOptions{command: "list"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("worktreesListCommand = %d, want 0; stderr %q", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "NAME") || !strings.Contains(out, "PATH") || !strings.Contains(out, "BRANCH") || !strings.Contains(out, "DIRTY") || !strings.Contains(out, "MERGED") {
		t.Fatalf("human output missing header: %q", out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("human output lines = %d, want 3 (header plus 2 entries): %q", len(lines), out)
	}
	if !strings.Contains(lines[1], "alpha") || !strings.Contains(lines[2], "bravo") {
		t.Fatalf("human output not sorted by name: %q", out)
	}
	if !strings.Contains(lines[1], "false") || !strings.Contains(lines[1], "run/alpha") {
		t.Fatalf("alpha line = %q, want dirty false and branch run/alpha", lines[1])
	}
	if !strings.Contains(lines[2], "true") || !strings.Contains(lines[2], "run/bravo") {
		t.Fatalf("bravo line = %q, want dirty true and branch run/bravo", lines[2])
	}
	if !strings.Contains(out, bravoPath) || !strings.Contains(out, alphaPath) {
		t.Fatalf("human output missing paths: %q", out)
	}
}

func TestWorktreesListJSON(t *testing.T) {
	repo := canonicalRepo(t, newGitWorkspace(t))
	bravoPath, _, err := prepareWorktree(context.Background(), repo, "bravo")
	if err != nil {
		t.Fatalf("prepareWorktree bravo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bravoPath, "file.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("dirty bravo: %v", err)
	}
	alphaPath, _, err := prepareWorktree(context.Background(), repo, "alpha")
	if err != nil {
		t.Fatalf("prepareWorktree alpha: %v", err)
	}
	if err := os.WriteFile(filepath.Join(alphaPath, "file.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatalf("write alpha: %v", err)
	}
	gitRun(t, alphaPath, "add", "file.txt")
	gitRun(t, alphaPath, "commit", "-q", "-m", "advance alpha")
	var stdout, stderr bytes.Buffer
	code := worktreesListCommand(context.Background(), worktreesOptions{command: "list", json: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("worktreesListCommand = %d, want 0; stderr %q", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	raw := strings.TrimSpace(stdout.String())
	if strings.Count(raw, "\n") != 0 {
		t.Fatalf("json output has multiple lines: %q", stdout.String())
	}
	var doc struct {
		SchemaVersion int `json:"schemaVersion"`
		Worktrees     []struct {
			Name   string `json:"name"`
			Path   string `json:"path"`
			Branch string `json:"branch"`
			Dirty  bool   `json:"dirty"`
			Merged bool   `json:"merged"`
		} `json:"worktrees"`
	}
	dec := json.NewDecoder(strings.NewReader(stdout.String()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode json: %v (%q)", err, stdout.String())
	}
	if doc.SchemaVersion != 1 {
		t.Fatalf("schemaVersion = %d, want 1", doc.SchemaVersion)
	}
	if len(doc.Worktrees) != 2 {
		t.Fatalf("worktrees len = %d, want 2", len(doc.Worktrees))
	}
	if doc.Worktrees[0].Name != "alpha" || doc.Worktrees[1].Name != "bravo" {
		t.Fatalf("worktrees order = %q %q, want alpha bravo", doc.Worktrees[0].Name, doc.Worktrees[1].Name)
	}
	if doc.Worktrees[0].Path != alphaPath || doc.Worktrees[1].Path != bravoPath {
		t.Fatalf("worktrees paths = %q %q, want %q %q", doc.Worktrees[0].Path, doc.Worktrees[1].Path, alphaPath, bravoPath)
	}
	if doc.Worktrees[0].Branch != "run/alpha" || doc.Worktrees[1].Branch != "run/bravo" {
		t.Fatalf("worktrees branches = %q %q", doc.Worktrees[0].Branch, doc.Worktrees[1].Branch)
	}
	if doc.Worktrees[0].Dirty != false || doc.Worktrees[1].Dirty != true {
		t.Fatalf("worktrees dirty = %v %v, want false true", doc.Worktrees[0].Dirty, doc.Worktrees[1].Dirty)
	}
	if doc.Worktrees[0].Merged != false || doc.Worktrees[1].Merged != true {
		t.Fatalf("worktrees merged = %v %v, want false true", doc.Worktrees[0].Merged, doc.Worktrees[1].Merged)
	}
	// Exact shape: one line, known keys only, [] not null for non-empty.
	if strings.Contains(stdout.String(), "\"worktrees\":null") {
		t.Fatalf("json contains null worktrees: %q", stdout.String())
	}
}

func TestWorktreesListEmpty(t *testing.T) {
	repo := canonicalRepo(t, newGitWorkspace(t))
	_ = repo
	var stdout, stderr bytes.Buffer
	code := worktreesListCommand(context.Background(), worktreesOptions{command: "list"}, &stdout, &stderr)
	if code != 0 || stdout.String() != "no managed worktrees\n" || stderr.Len() != 0 {
		t.Fatalf("empty human = (%d, %q, %q), want (0, \"no managed worktrees\\n\", \"\")", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = worktreesListCommand(context.Background(), worktreesOptions{command: "list", json: true}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("empty json = (%d, %q, %q), want exit 0", code, stdout.String(), stderr.String())
	}
	if stdout.String() != "{\"schemaVersion\":1,\"worktrees\":[]}\n" {
		t.Fatalf("empty json = %q, want %q", stdout.String(), "{\"schemaVersion\":1,\"worktrees\":[]}\n")
	}
}

func TestWorktreesListInventoryFailure(t *testing.T) {
	repo := canonicalRepo(t, newGitWorkspace(t))
	withRunGitFunc(t, func(ctx context.Context, dir string, args ...string) (string, error) {
		return "", os.ErrInvalid
	})
	_ = repo
	var stdout, stderr bytes.Buffer
	code := worktreesListCommand(context.Background(), worktreesOptions{command: "list"}, &stdout, &stderr)
	if code != 9 {
		t.Fatalf("inventory failure = %d, want 9; stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty on inventory failure", stdout.String())
	}
	if !strings.Contains(stderr.String(), "pi-worker:") {
		t.Fatalf("stderr = %q, want it to contain pi-worker:", stderr.String())
	}
}

func TestWorktreesListPreservesSpaces(t *testing.T) {
	repo := canonicalRepo(t, newGitWorkspaceAt(t, filepath.Join(t.TempDir(), "repo with spaces")))
	if _, _, err := prepareWorktree(context.Background(), repo, "spacey"); err != nil {
		t.Fatalf("prepareWorktree: %v", err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := worktreesListCommand(context.Background(), worktreesOptions{command: "list", json: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("worktreesListCommand = %d, want 0; stderr %q", code, stderr.String())
	}
	var doc struct {
		Worktrees []struct {
			Path string `json:"path"`
		} `json:"worktrees"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	if len(doc.Worktrees) != 1 || !strings.Contains(doc.Worktrees[0].Path, "repo with spaces") {
		t.Fatalf("worktrees path = %#v, want it to contain spaces", doc.Worktrees)
	}
	stdout.Reset()
	stderr.Reset()
	code = worktreesListCommand(context.Background(), worktreesOptions{command: "list"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("human worktreesListCommand = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "repo with spaces") {
		t.Fatalf("human output = %q, want it to contain spaced path", stdout.String())
	}
}

func TestWorktreesListDispatchedViaMain(t *testing.T) {
	repo := canonicalRepo(t, newGitWorkspace(t))
	if _, _, err := prepareWorktree(context.Background(), repo, "alpha"); err != nil {
		t.Fatalf("prepareWorktree: %v", err)
	}
	code, stdout, stderr := runCLI(t, []string{"worktrees", "list"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("worktrees list = (%d, %q, %q), want exit 0", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "NAME") || !strings.Contains(stdout, "alpha") {
		t.Fatalf("human stdout = %q, want header and alpha", stdout)
	}
	code, stdout, stderr = runCLI(t, []string{"worktrees", "list", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("worktrees list --json = (%d, %q, %q)", code, stdout, stderr)
	}
	if strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
		t.Fatalf("json output has multiple lines: %q", stdout)
	}
	var doc struct {
		SchemaVersion int `json:"schemaVersion"`
		Worktrees     []struct {
			Name string `json:"name"`
		} `json:"worktrees"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("decode json %q: %v", stdout, err)
	}
	if doc.SchemaVersion != 1 || len(doc.Worktrees) != 1 || doc.Worktrees[0].Name != "alpha" {
		t.Fatalf("document = %#v", doc)
	}
}

func TestWorktreesListUsageErrorsViaMain(t *testing.T) {
	for _, args := range [][]string{
		{"worktrees"},
		{"worktrees", "prune"},
		{"worktrees", "list", "--json=1"},
		{"worktrees", "list", "--bogus"},
		{"worktrees", "list", "extra"},
		{"worktrees", "--json", "list"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, stdout, stderr := runCLI(t, args, "")
			if code != 2 || stdout != "" || !strings.Contains(stderr, "pi-worker:") {
				t.Fatalf("%v = (%d, %q, %q), want exit 2 with pi-worker: on stderr and no stdout", args, code, stdout, stderr)
			}
			if !strings.Contains(stderr, "usage:") {
				t.Fatalf("stderr = %q, want usage text", stderr)
			}
		})
	}
}

func TestWorktreesListWiredIntoMainWithContext(t *testing.T) {
	repo := canonicalRepo(t, newGitWorkspace(t))
	if _, _, err := prepareWorktree(context.Background(), repo, "alpha"); err != nil {
		t.Fatalf("prepareWorktree: %v", err)
	}
	code, stdout, stderr := runCLIWithContext(t, context.Background(), []string{"worktrees", "list", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("mainWithContext worktrees list = (%d, %q, %q)", code, stdout, stderr)
	}
	var doc struct {
		SchemaVersion int `json:"schemaVersion"`
		Worktrees     []struct {
			Name string `json:"name"`
		} `json:"worktrees"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	if doc.SchemaVersion != 1 || len(doc.Worktrees) != 1 || doc.Worktrees[0].Name != "alpha" {
		t.Fatalf("document = %#v", doc)
	}
}

func TestMainUsageIncludesWorktreesCommand(t *testing.T) {
	code, stdout, stderr := runCLI(t, []string{}, "")
	if code != 2 || stdout != "" {
		t.Fatalf("empty argv = (%d, %q)", code, stdout)
	}
	if !strings.Contains(stderr, "pi-worker worktrees list [--json]") {
		t.Fatalf("usage missing worktrees list: %q", stderr)
	}
	if !strings.Contains(stderr, "pi-worker worktrees remove <name> [--yes] [--json]") {
		t.Fatalf("usage missing worktrees remove: %q", stderr)
	}
}

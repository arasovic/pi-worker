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
// options they parse into: the bare list subcommand, and list with the
// single --json flag following the subcommand. The subcommand must be
// first; only --json is a flag today.
func TestParseWorktreesArgsAccepted(t *testing.T) {
	for _, test := range []struct {
		args []string
		want worktreesOptions
	}{
		{args: []string{"list"}, want: worktreesOptions{command: "list"}},
		{args: []string{"list", "--json"}, want: worktreesOptions{command: "list", json: true}},
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
// extra positional argument, a --json flag given a value, and a
// repeated --json.
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
		{args: []string{"prune"}, want: "unknown worktrees command"},
		{args: []string{"--json", "list"}, want: "unknown worktrees command"},
		{args: []string{"--json", "--json", "list"}, want: "unknown worktrees command"},
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

package cli

import (
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

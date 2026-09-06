package worktree

import (
	"path/filepath"
	"strings"
	"testing"
)

// --- parseManagedWorktreeList ---

func TestParseManagedWorktreeList(t *testing.T) {
	root := t.TempDir()
	managed := func(name string) string {
		return filepath.Join(root, ".pi-worker", "worktrees", name)
	}

	tests := []struct {
		name    string
		output  string
		want    map[string]entryRef
		wantErr string
	}{
		{
			name:   "empty",
			output: "",
			want:   map[string]entryRef{},
		},
		{
			name:   "one managed entry",
			output: "worktree " + managed("alpha") + "\nbranch refs/heads/run/alpha\n",
			want:   map[string]entryRef{"alpha": {path: managed("alpha"), branch: "run/alpha"}},
		},
		{
			name: "two managed entries sorted",
			output: "worktree " + managed("bravo") + "\nbranch refs/heads/run/bravo\n\n" +
				"worktree " + managed("alpha") + "\nbranch refs/heads/run/alpha\n",
			want: map[string]entryRef{
				"alpha": {path: managed("alpha"), branch: "run/alpha"},
				"bravo": {path: managed("bravo"), branch: "run/bravo"},
			},
		},
		{
			name:   "unrelated worktree ignored",
			output: "worktree /tmp/other\nbranch refs/heads/feature\n\nworktree " + managed("alpha") + "\nbranch refs/heads/run/alpha\n",
			want:   map[string]entryRef{"alpha": {path: managed("alpha"), branch: "run/alpha"}},
		},
		{
			name: "unrelated with detached bare locked prunable ignored",
			output: "worktree /tmp/other\ndetached\nbare\nlocked unrelated\nprunable stale\n\n" +
				"worktree " + managed("probe") + "\nbranch refs/heads/run/probe\n",
			want: map[string]entryRef{"probe": {path: managed("probe"), branch: "run/probe"}},
		},
		{
			name:    "missing path line (branch before flush)",
			output:  "branch refs/heads/run/alpha\n",
			wantErr: "entry missing worktree path",
		},
		{
			name:    "missing branch for managed",
			output:  "worktree " + managed("alpha") + "\n",
			wantErr: "missing its branch",
		},
		{
			name:    "detached managed has no branch",
			output:  "worktree " + managed("alpha") + "\ndetached\n",
			wantErr: "missing its branch",
		},
		{
			name:    "mismatched branch",
			output:  "worktree " + managed("alpha") + "\nbranch refs/heads/run/beta\n",
			wantErr: `points to "refs/heads/run/beta"`,
		},
		{
			name:    "bare managed",
			output:  "worktree " + managed("alpha") + "\nbranch refs/heads/run/alpha\nbare\n",
			wantErr: "is bare",
		},
		{
			name:    "locked managed",
			output:  "worktree " + managed("alpha") + "\nbranch refs/heads/run/alpha\nlocked\n",
			wantErr: "is locked",
		},
		{
			name:    "prunable managed",
			output:  "worktree " + managed("alpha") + "\nbranch refs/heads/run/alpha\nprunable\n",
			wantErr: "is prunable",
		},
		{
			name:    "duplicate worktree line",
			output:  "worktree " + managed("alpha") + "\nworktree " + managed("alpha") + "\nbranch refs/heads/run/alpha\n",
			wantErr: "duplicate worktree line",
		},
		{
			name:    "duplicate branch line",
			output:  "worktree " + managed("alpha") + "\nbranch refs/heads/run/alpha\nbranch refs/heads/run/alpha\n",
			wantErr: "duplicate branch line",
		},
		{
			name: "duplicate managed checkout across entries",
			output: "worktree " + managed("alpha") + "\nbranch refs/heads/run/alpha\n\n" +
				"worktree " + managed("alpha") + "\nbranch refs/heads/run/alpha\n",
			wantErr: "duplicate managed checkout",
		},
		{
			name:    "unexpected porcelain line",
			output:  "worktree " + managed("alpha") + "\nbranch refs/heads/run/alpha\nbogus-key value\n",
			wantErr: "unexpected line",
		},
		{
			name:    "overlong line rejected",
			output:  "worktree " + strings.Repeat("x", 128*1024) + "\n",
			wantErr: "token too long",
		},
		{
			name:    "too many entries",
			output:  strings.Repeat("worktree /tmp/x\nbranch refs/heads/run/x\n\n", 4097),
			wantErr: "too many entries",
		},
		{
			name: "HEAD line ignored",
			output: "worktree " + managed("alpha") + "\nbranch refs/heads/run/alpha\n" +
				"HEAD abc123\n",
			want: map[string]entryRef{"alpha": {path: managed("alpha"), branch: "run/alpha"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseManagedWorktreeList(root, tt.output)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("got nil error, want it to contain %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d entries, want %d: %v", len(got), len(tt.want), got)
			}
			for k, wantRef := range tt.want {
				gotRef, ok := got[k]
				if !ok {
					t.Fatalf("missing key %q in got %v", k, got)
				}
				if gotRef != wantRef {
					t.Fatalf("got[%q] = %v, want %v", k, gotRef, wantRef)
				}
			}
		})
	}
}

// --- managedNameFromPath ---

func TestManagedNameFromPath(t *testing.T) {
	dir := filepath.Join("/repo", ".pi-worker", "worktrees")
	tests := []struct {
		name     string
		path     string
		wantName string
		wantMgd  bool
		wantErr  string
	}{
		{"exact child", filepath.Join(dir, "probe"), "probe", true, ""},
		{"outside path", "/other/path", "", false, ""},
		{"parent path", filepath.Join(dir, ".."), "", false, ""},
		{"missing name (is root)", dir, "", false, "missing a name"},
		{"nested path", filepath.Join(dir, "a", "b"), "", false, "nested"},
		{"invalid name", filepath.Join(dir, "Bad_Name"), "", false, "invalid name"},
		{"relative path", "relative/path", "", false, "not absolute"},
		{"unclean path", dir + "/./probe", "", false, "not clean"},
		{"long valid name", filepath.Join(dir, strings.Repeat("a", 64)), strings.Repeat("a", 64), true, ""},
		{"overlong name rejected by ValidName", filepath.Join(dir, strings.Repeat("a", 65)), "", false, "invalid name"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, gotMgd, err := managedNameFromPath(dir, tt.path)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("got nil error, want it to contain %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
				}
				if gotMgd {
					t.Fatalf("managed = true, want false on error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotName != tt.wantName {
				t.Fatalf("name = %q, want %q", gotName, tt.wantName)
			}
			if gotMgd != tt.wantMgd {
				t.Fatalf("managed = %v, want %v", gotMgd, tt.wantMgd)
			}
		})
	}
}

// --- parseManagedBranchNames ---

func TestParseManagedBranchNames(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		want    map[string]struct{}
		wantErr string
	}{
		{
			name:   "empty",
			output: "",
			want:   map[string]struct{}{},
		},
		{
			name:   "one managed branch",
			output: "run/alpha\n",
			want:   map[string]struct{}{"alpha": {}},
		},
		{
			name:   "multiple managed branches",
			output: "run/alpha\nrun/bravo\nrun/charlie\n",
			want:   map[string]struct{}{"alpha": {}, "bravo": {}, "charlie": {}},
		},
		{
			name:   "unrelated branches ignored",
			output: "feature\nrun/alpha\nother\n",
			want:   map[string]struct{}{"alpha": {}},
		},
		{
			name:   "blank lines skipped",
			output: "run/alpha\n\n\nrun/bravo\n",
			want:   map[string]struct{}{"alpha": {}, "bravo": {}},
		},
		{
			name:    "invalid managed name",
			output:  "run/Bad\n",
			wantErr: "invalid name",
		},
		{
			name:    "duplicate managed branch",
			output:  "run/alpha\nrun/alpha\n",
			wantErr: "duplicate managed branch",
		},
		{
			name:    "too many entries",
			output:  strings.Repeat("feature\n", 4096) + "run/alpha\n",
			wantErr: "too many entries",
		},
		{
			name:    "overlong line rejected",
			output:  "run/" + strings.Repeat("x", 128*1024) + "\n",
			wantErr: "token too long",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseManagedBranchNames(tt.output)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("got nil error, want it to contain %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d entries, want %d", len(got), len(tt.want))
			}
			for k := range tt.want {
				if _, ok := got[k]; !ok {
					t.Fatalf("missing key %q in got %v", k, got)
				}
			}
		})
	}
}

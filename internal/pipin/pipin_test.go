package pipin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFixture writes the given files under root, creating parent
// directories, and returns root.
func writeFixture(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for path, content := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

// fixtureTree builds a repository-shaped fixture with a pin of 1.2.3 and
// returns its root. The stale sites use 1.2.4 and 9.9.9, and the fixture also
// carries the shapes the walker must leave alone: a version before [Pi] on
// the same line, the excluded docs/pi-cli-surface.md path, and the skipped
// node_modules and dist directories.
func fixtureTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFixture(t, root, map[string]string{
		"compat/pi/package.json": `{
  "name": "pi-worker-compat-pi",
  "private": true,
  "description": "Pins the Pi release pi-worker is verified against. Not installed; it exists so Dependabot can watch the version.",
  "dependencies": {
    "@earendil-works/pi-coding-agent": "1.2.3"
  }
}
`,
		"internal/piversion/version.go": `package piversion

const (
	// VerifiedVersion is the only Pi version verified by pi-worker.
	VerifiedVersion = "1.2.4"
)
`,
		"docs/guide.md": `# Pi compatibility guide
Pi 1.2.3 is the verified pin and matches.
Pi 9.9.9 is stale and must be rewritten.
Node.js 22.20.0 or newer and a [Pi](https://pi.dev/) CLI with provider login.
Pi **6.6.6** wrapped in bold gets its token replaced.
The unmarked **9.9.9** token here is not a site.
Pi ` + "`7.7.7`" + ` wrapped in backticks likewise.
`,
		"docs/pi-cli-surface.md": `# Observed surface
Pi 9.9.9 was observed by an earlier probe. Rewriting this file is forbidden.
`,
		"docs/deep/notes.md": `# Nested notes
Pi 1.2.7 is stale deep in the tree.
`,
		"README.md": `# Fixture readme
Pi 1.2.3 matches the pin already.
`,
		"node_modules/dep/README.md": "Pi 5.5.5 inside node_modules is skipped.\n",
		"dist/out.md":                "Pi 5.5.5 inside dist is skipped.\n",
	})
	return root
}

func TestReadPin(t *testing.T) {
	t.Run("returns the pinned version", func(t *testing.T) {
		root := fixtureTree(t)
		pin, err := ReadPin(root)
		if err != nil {
			t.Fatalf("ReadPin: %v", err)
		}
		if pin != "1.2.3" {
			t.Fatalf("ReadPin = %q, want %q", pin, "1.2.3")
		}
	})

	t.Run("missing manifest", func(t *testing.T) {
		root := t.TempDir()
		if _, err := ReadPin(root); err == nil || !strings.Contains(err.Error(), "compat/pi/package.json") {
			t.Fatalf("ReadPin error = %v, want one naming compat/pi/package.json", err)
		}
	})

	t.Run("missing dependency key", func(t *testing.T) {
		root := t.TempDir()
		writeFixture(t, root, map[string]string{
			"compat/pi/package.json": `{
  "dependencies": {
    "other": "1.2.3"
  }
}
`,
		})
		if _, err := ReadPin(root); err == nil || !strings.Contains(err.Error(), "@earendil-works/pi-coding-agent") {
			t.Fatalf("ReadPin error = %v, want one naming the missing dependency key", err)
		}
	})

	t.Run("version range is not a bare semantic version", func(t *testing.T) {
		for _, value := range []string{"latest", "^1.2.3", "~1.2", "v1.2.3", "1.2.3.4"} {
			root := t.TempDir()
			writeFixture(t, root, map[string]string{
				"compat/pi/package.json": `{
  "dependencies": {
    "@earendil-works/pi-coding-agent": "` + value + `"
  }
}
`,
			})
			if _, err := ReadPin(root); err == nil || !strings.Contains(err.Error(), "not a bare semantic version") {
				t.Fatalf("ReadPin with %q error = %v, want one naming the invalid value", value, err)
			}
		}
	})
}

func TestCheckReportsOnlyStaleSites(t *testing.T) {
	root := fixtureTree(t)
	reports, err := Check(root)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	want := []string{
		"internal/piversion/version.go:5: version 1.2.4, pin is 1.2.3",
		"docs/deep/notes.md:2: version 1.2.7, pin is 1.2.3",
		"docs/guide.md:3: version 9.9.9, pin is 1.2.3",
		"docs/guide.md:5: version 6.6.6, pin is 1.2.3",
		"docs/guide.md:7: version 7.7.7, pin is 1.2.3",
	}
	if len(reports) != len(want) {
		t.Fatalf("Check reports = %#v, want %#v", reports, want)
	}
	for i := range want {
		if reports[i] != want[i] {
			t.Fatalf("Check report %d = %q, want %q", i, reports[i], want[i])
		}
	}
}

func TestWriteRewritesStaleSitesAndIsIdempotent(t *testing.T) {
	root := fixtureTree(t)

	changed, err := Write(root)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(changed) != 5 {
		t.Fatalf("Write changed %d sites, want 5: %#v", len(changed), changed)
	}

	// Every expectation here is a literal of the content the pin 1.2.3 must
	// produce; none of it is read back from the code under test.
	expect := map[string]string{
		"internal/piversion/version.go": `package piversion

const (
	// VerifiedVersion is the only Pi version verified by pi-worker.
	VerifiedVersion = "1.2.3"
)
`,
		"docs/guide.md": `# Pi compatibility guide
Pi 1.2.3 is the verified pin and matches.
Pi 1.2.3 is stale and must be rewritten.
Node.js 22.20.0 or newer and a [Pi](https://pi.dev/) CLI with provider login.
Pi **1.2.3** wrapped in bold gets its token replaced.
The unmarked **9.9.9** token here is not a site.
Pi ` + "`1.2.3`" + ` wrapped in backticks likewise.
`,
		"docs/pi-cli-surface.md": `# Observed surface
Pi 9.9.9 was observed by an earlier probe. Rewriting this file is forbidden.
`,
		"docs/deep/notes.md": `# Nested notes
Pi 1.2.3 is stale deep in the tree.
`,
		"README.md": `# Fixture readme
Pi 1.2.3 matches the pin already.
`,
		"node_modules/dep/README.md": "Pi 5.5.5 inside node_modules is skipped.\n",
		"dist/out.md":                "Pi 5.5.5 inside dist is skipped.\n",
	}
	for path, want := range expect {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatalf("read %s after Write: %v", path, err)
		}
		if string(data) != want {
			t.Fatalf("%s after Write:\n got %q\nwant %q", path, string(data), want)
		}
	}

	// A second Write rewrites nothing and changes no bytes.
	again, err := Write(root)
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second Write changed %d sites, want 0: %#v", len(again), again)
	}
	for path := range expect {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatalf("read %s after second Write: %v", path, err)
		}
		if string(data) != expect[path] {
			t.Fatalf("%s changed between the two Write calls", path)
		}
	}

	// After the rewrite the tree is clean.
	reports, err := Check(root)
	if err != nil {
		t.Fatalf("Check after Write: %v", err)
	}
	if len(reports) != 0 {
		t.Fatalf("Check after Write reports %#v, want none", reports)
	}
}

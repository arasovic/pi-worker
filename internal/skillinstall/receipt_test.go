package skillinstall

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLoadRejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	root := t.TempDir()
	base := mustMarshalReceipt(t, validReceipt(filepath.Join(root, "target"), OutcomeInstalled))

	t.Run("unknown field", func(t *testing.T) {
		var raw map[string]any
		if err := json.Unmarshal(base, &raw); err != nil {
			t.Fatalf("unmarshal base receipt: %v", err)
		}
		raw["unexpected"] = true
		path := writeReceiptBytes(t, root, mustMarshalJSON(raw))
		if _, err := Load(path); err == nil {
			t.Fatalf("Load() = nil, want error")
		}
	})

	t.Run("trailing bytes", func(t *testing.T) {
		path := writeReceiptBytes(t, root, append(base, []byte("abc")...))
		if _, err := Load(path); err == nil {
			t.Fatalf("Load() = nil, want error")
		}
	})
}

func TestLoadRejectsMalformedStructure(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	affected := filepath.Join(root, "pi-worker")

	cases := []struct {
		name string
		json []byte
	}{
		{
			name: "wrong schema",
			json: mustMarshalReceipt(t, modifyReceipt(validReceipt(target, OutcomeInstalled), func(r *Receipt) {
				r.SchemaVersion = 2
			})),
		},
		{
			name: "missing schema",
			json: []byte(`{"installerVersion":"1","skillsVersion":"1","outcome":"installed","targets":[],"affectedTargets":[],"recovery":[]}`),
		},
		{
			name: "schema wrong type",
			json: []byte(`{"schemaVersion":"1","installerVersion":"1","skillsVersion":"1","outcome":"installed","targets":[],"affectedTargets":[],"recovery":[]}`),
		},
		{
			name: "unknown outcome",
			json: mustMarshalReceipt(t, modifyReceipt(validReceipt(target, OutcomeInstalled), func(r *Receipt) {
				r.Outcome = Outcome("bad")
			})),
		},
		{
			name: "unknown kind",
			json: mustMarshalReceipt(t, modifyReceipt(validReceipt(target, OutcomeInstalled), func(r *Receipt) {
				r.Targets[0].Kind = "bad"
			})),
		},
		{
			name: "malformed sha256",
			json: mustMarshalReceipt(t, modifyReceipt(validReceipt(target, OutcomeInstalled), func(r *Receipt) {
				r.Targets[0].Files[0].SHA256 = "not-a-sha"
			})),
		},
		{
			name: "relative target",
			json: mustMarshalReceipt(t, modifyReceipt(validReceipt(target, OutcomeInstalled), func(r *Receipt) {
				r.Targets[0].Path = "relative"
			})),
		},
		{
			name: "absolute file path",
			json: mustMarshalReceipt(t, modifyReceipt(validReceipt(target, OutcomeInstalled), func(r *Receipt) {
				r.Targets[0].Files[0].Path = "/abs.txt"
			})),
		},
		{
			name: "traversal file path",
			json: mustMarshalReceipt(t, modifyReceipt(validReceipt(target, OutcomeInstalled), func(r *Receipt) {
				r.Targets[0].Files[0].Path = "../one.txt"
			})),
		},
		{
			name: "duplicate file paths",
			json: mustMarshalReceipt(t, Receipt{
				SchemaVersion:    SchemaVersion,
				InstallerVersion: "1",
				SkillsVersion:    "1",
				Outcome:          OutcomeInstalled,
				Targets: []Target{{
					Path:  target,
					Kind:  targetKindCanonical,
					Files: []FileHash{{Path: "one.txt", SHA256: strings.Repeat("0", 64)}, {Path: "./one.txt", SHA256: strings.Repeat("0", 64)}},
				}},
			}),
		},
		{
			name: "non-absolute affected path",
			json: mustMarshalReceipt(t, Receipt{
				SchemaVersion:    SchemaVersion,
				InstallerVersion: "1",
				SkillsVersion:    "1",
				Outcome:          OutcomeBlocked,
				Targets: []Target{{
					Path:  target,
					Kind:  targetKindCanonical,
					Files: []FileHash{{Path: "one.txt", SHA256: strings.Repeat("0", 64)}},
				}},
				AffectedTargets: []AffectedTarget{{Path: "relative", State: AffectedDrifted, Recovery: []string{"backup"}}},
				Recovery:        []string{"global"},
			}),
		},
		{
			name: "duplicate affected path",
			json: mustMarshalReceipt(t, Receipt{
				SchemaVersion:    SchemaVersion,
				InstallerVersion: "1",
				SkillsVersion:    "1",
				Outcome:          OutcomeBlocked,
				Targets: []Target{{
					Path:  target,
					Kind:  targetKindCanonical,
					Files: []FileHash{{Path: "one.txt", SHA256: strings.Repeat("0", 64)}},
				}},
				AffectedTargets: []AffectedTarget{{Path: affected, State: AffectedUnmanaged, Recovery: []string{"a"}}, {Path: filepath.Clean(filepath.Join(affected, ".")), State: AffectedDrifted, Recovery: []string{"b"}}},
				Recovery:        []string{"global"},
			}),
		},
		{
			name: "unknown affected state",
			json: mustMarshalReceipt(t, Receipt{
				SchemaVersion:    SchemaVersion,
				InstallerVersion: "1",
				SkillsVersion:    "1",
				Outcome:          OutcomeBlocked,
				Targets: []Target{{
					Path:  target,
					Kind:  targetKindCanonical,
					Files: []FileHash{{Path: "one.txt", SHA256: strings.Repeat("0", 64)}},
				}},
				AffectedTargets: []AffectedTarget{{Path: affected, State: AffectedState("bad"), Recovery: []string{"a"}}},
				Recovery:        []string{"global"},
			}),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeReceiptBytes(t, root, tc.json)
			if _, err := Load(path); err == nil {
				t.Fatalf("Load() = nil, want error: %s", tc.name)
			}
		})
	}
}

func TestLoadRejectsOutcomeRules(t *testing.T) {
	caseRoot := t.TempDir()
	path := filepath.Join(caseRoot, "target")

	t.Run("installed with affected", func(t *testing.T) {
		receipt := validReceipt(path, OutcomeInstalled)
		receipt.AffectedTargets = []AffectedTarget{{Path: filepath.Join(path, "pi-worker"), State: AffectedUnmanaged, Recovery: []string{"x"}}}
		if _, err := Load(writeReceiptFromReceipt(t, caseRoot, receipt)); err == nil {
			t.Fatalf("Load() = nil, want error")
		}
	})
	t.Run("blocked without affected", func(t *testing.T) {
		receipt := validReceipt(path, OutcomeBlocked)
		receipt.AffectedTargets = nil
		if _, err := Load(writeReceiptFromReceipt(t, caseRoot, receipt)); err == nil {
			t.Fatalf("Load() = nil, want error")
		}
	})
}

func TestLoadAcceptsConflictingBlockedStateWithGlobalRecovery(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	receipt := Receipt{
		SchemaVersion:    SchemaVersion,
		InstallerVersion: "1",
		SkillsVersion:    "1",
		Outcome:          OutcomeBlocked,
		Targets:          []Target{{Path: target, Kind: targetKindCanonical, Files: []FileHash{{Path: "marker", SHA256: strings.Repeat("0", 64)}}}},
		AffectedTargets:  []AffectedTarget{{Path: filepath.Join(t.TempDir(), "other"), State: AffectedConflicting, Recovery: []string{"path backup"}}},
		Recovery:         []string{"global remove"},
	}
	if _, err := Load(writeReceiptFromReceipt(t, t.TempDir(), receipt)); err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
}

func TestInspectVerifiesAndClassifiesTargets(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "canonical")
	writeFile(t, filepath.Join(target, "a.txt"), "one")
	writeFile(t, filepath.Join(target, IdentityFile), IdentityContent)
	writeFile(t, filepath.Join(target, "SKILL.md"), "---\nname: pi-worker\n---\n")

	receipt := Receipt{
		SchemaVersion:    SchemaVersion,
		InstallerVersion: "1",
		SkillsVersion:    PinnedSkillsVersion,
		Outcome:          OutcomeInstalled,
		Targets: []Target{{
			Path: target,
			Kind: targetKindCanonical,
			Files: []FileHash{
				{Path: "a.txt", SHA256: hashString("one")},
				{Path: IdentityFile, SHA256: hashString(IdentityContent)},
				{Path: "SKILL.md", SHA256: hashString("---\nname: pi-worker\n---\n")},
			},
		}},
	}
	inspection, err := Inspect(writeReceiptFromReceipt(t, root, receipt))
	if err != nil {
		t.Fatalf("Inspect() = %v, want nil", err)
	}
	if inspection.Status != StatusVerified {
		t.Fatalf("status = %q, want %q", inspection.Status, StatusVerified)
	}
	expected := []string{filepath.Clean(target)}
	if !equalStringSlice(inspection.VerifiedTargets, expected) {
		t.Fatalf("verified targets = %v, want %v", inspection.VerifiedTargets, expected)
	}

	t.Run("additional owned targets do not replace the single canonical identity", func(t *testing.T) {
		copyTarget := filepath.Join(root, "copy")
		writeFile(t, filepath.Join(copyTarget, "copy.txt"), "copy")
		withCopy := receipt
		withCopy.Targets = append(append([]Target{}, receipt.Targets...), Target{
			Path:  copyTarget,
			Kind:  targetKindCopy,
			Files: []FileHash{{Path: "copy.txt", SHA256: hashString("copy")}},
		})
		inspection, err := Inspect(writeReceiptFromReceipt(t, t.TempDir(), withCopy))
		if err != nil {
			t.Fatalf("Inspect() = %v, want nil", err)
		}
		if inspection.Status != StatusVerified {
			t.Fatalf("status = %q, want %q", inspection.Status, StatusVerified)
		}
		want := []string{filepath.Clean(target), filepath.Clean(copyTarget)}
		if !equalStringSlice(inspection.VerifiedTargets, want) {
			t.Fatalf("verified targets = %v, want %v", inspection.VerifiedTargets, want)
		}
	})

	t.Run("missing target", func(t *testing.T) {
		receipt := Receipt{
			SchemaVersion:    SchemaVersion,
			InstallerVersion: "1",
			SkillsVersion:    "1",
			Outcome:          OutcomeInstalled,
			Targets: []Target{{
				Path:  filepath.Join(root, "missing"),
				Kind:  targetKindCanonical,
				Files: []FileHash{{Path: "x.txt", SHA256: hashString("x")}},
			}},
		}
		inspection, err := Inspect(writeReceiptFromReceipt(t, root, receipt))
		if err != nil {
			t.Fatalf("Inspect() = %v, want nil", err)
		}
		if inspection.Status != StatusMissing {
			t.Fatalf("status = %q, want %q", inspection.Status, StatusMissing)
		}
		if !equalStringSlice(inspection.Recovery, []string{SafeRecoveryCommand}) {
			t.Fatalf("recovery = %v, want safe recovery", inspection.Recovery)
		}
	})

	t.Run("drifted target", func(t *testing.T) {
		receipt := Receipt{
			SchemaVersion:    SchemaVersion,
			InstallerVersion: "1",
			SkillsVersion:    "1",
			Outcome:          OutcomeInstalled,
			Targets: []Target{{
				Path:  target,
				Kind:  targetKindCanonical,
				Files: []FileHash{{Path: "a.txt", SHA256: hashString("different")}},
			}},
		}
		inspection, err := Inspect(writeReceiptFromReceipt(t, root, receipt))
		if err != nil {
			t.Fatalf("Inspect() = %v, want nil", err)
		}
		if inspection.Status != StatusDrifted {
			t.Fatalf("status = %q, want %q", inspection.Status, StatusDrifted)
		}
	})
}

func TestInspectRejectsHashMatchingForeignReceipt(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "foreign")
	writeFile(t, filepath.Join(target, "foreign.txt"), "foreign")
	receipt := Receipt{
		SchemaVersion:    SchemaVersion,
		InstallerVersion: "1",
		SkillsVersion:    PinnedSkillsVersion,
		Outcome:          OutcomeInstalled,
		Targets: []Target{{
			Path:  target,
			Kind:  targetKindCanonical,
			Files: []FileHash{{Path: "foreign.txt", SHA256: hashString("foreign")}},
		}},
	}
	inspection, err := Inspect(writeReceiptFromReceipt(t, root, receipt))
	if err != nil {
		t.Fatalf("Inspect() = %v, want nil", err)
	}
	if inspection.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", inspection.Status, StatusFailed)
	}
	if inspection.VerifiedTargets == nil || len(inspection.VerifiedTargets) != 0 {
		t.Fatalf("verified targets = %v, want non-nil empty", inspection.VerifiedTargets)
	}
}

func TestInspectDoesNotExposeGlobalRecoveryForStaleExtraDriftAttribution(t *testing.T) {
	root := t.TempDir()
	managed := filepath.Join(root, "pi-worker")
	stale := filepath.Join(root, "stale")
	writeFile(t, filepath.Join(managed, "tool.txt"), "managed")
	writeFile(t, filepath.Join(managed, IdentityFile), IdentityContent)
	writeFile(t, filepath.Join(managed, "SKILL.md"), "---\nname: pi-worker\n---\n")
	writeFile(t, filepath.Join(stale, "tool.txt"), "stale")
	receipt := Receipt{
		SchemaVersion:    SchemaVersion,
		InstallerVersion: "1",
		SkillsVersion:    PinnedSkillsVersion,
		Outcome:          OutcomeBlocked,
		Targets: []Target{{
			Path:  managed,
			Kind:  targetKindCanonical,
			Files: []FileHash{{Path: "tool.txt", SHA256: hashString("managed")}},
		}, {
			Path:  stale,
			Kind:  targetKindCanonical,
			Files: []FileHash{{Path: "tool.txt", SHA256: hashString("stale")}},
		}},
		AffectedTargets: []AffectedTarget{{Path: managed, State: AffectedUnmanaged, Recovery: []string{}}, {Path: stale, State: AffectedDrifted, Recovery: []string{}}},
		Recovery:        []string{"npx --yes skills@" + PinnedSkillsVersion + " remove pi-worker -g -y", SafeRecoveryCommand},
	}
	inspection, err := Inspect(writeReceiptFromReceipt(t, root, receipt))
	if err != nil {
		t.Fatalf("Inspect() = %v, want nil", err)
	}
	if len(inspection.Recovery) != 0 {
		t.Fatalf("recovery = %v, want empty", inspection.Recovery)
	}
}

func TestInspectExactManifestRejectsExtraFilesAndEmptyDirectories(t *testing.T) {
	for _, kind := range []string{targetKindCanonical, targetKindCopy} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "target")
			writeFile(t, filepath.Join(target, "expected.txt"), "expected")
			writeFile(t, filepath.Join(target, "extra.txt"), "extra")
			if err := os.Mkdir(filepath.Join(target, "empty"), 0o700); err != nil {
				t.Fatalf("mkdir empty directory: %v", err)
			}
			receipt := validReceipt(target, OutcomeInstalled)
			receipt.Targets[0].Kind = kind
			receipt.Targets[0].Files = []FileHash{{Path: "expected.txt", SHA256: hashString("expected")}}
			inspection, err := Inspect(writeReceiptFromReceipt(t, root, receipt))
			if err != nil {
				t.Fatalf("Inspect() = %v", err)
			}
			if inspection.Status != StatusDrifted {
				t.Fatalf("status = %q, want %q", inspection.Status, StatusDrifted)
			}
		})
	}
}

func TestInspectExactManifestRejectsSymlinkedIntermediateDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip symlink test on windows")
	}
	root := t.TempDir()
	target := filepath.Join(root, "target")
	realDir := filepath.Join(root, "real")
	writeFile(t, filepath.Join(realDir, "tool.txt"), "tool")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.Symlink(realDir, filepath.Join(target, "nested")); err != nil {
		t.Fatalf("symlink intermediate directory: %v", err)
	}
	receipt := validReceipt(target, OutcomeInstalled)
	receipt.Targets[0].Files = []FileHash{{Path: "nested/tool.txt", SHA256: hashString("tool")}}
	inspection, err := Inspect(writeReceiptFromReceipt(t, root, receipt))
	if err != nil {
		t.Fatalf("Inspect() = %v", err)
	}
	if inspection.Status != StatusDrifted {
		t.Fatalf("status = %q, want %q", inspection.Status, StatusDrifted)
	}
}

func TestInspectExactManifestReadsDirectoriesInBoundedBatches(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	writeFile(t, filepath.Join(target, "expected.txt"), "expected")
	for i := 0; i <= directoryReadBatchSize; i++ {
		writeFile(t, filepath.Join(target, fmt.Sprintf("extra-%03d.txt", i)), "extra")
	}
	receipt := validReceipt(target, OutcomeInstalled)
	receipt.Targets[0].Files = []FileHash{{Path: "expected.txt", SHA256: hashString("expected")}}
	inspection, err := Inspect(writeReceiptFromReceipt(t, root, receipt))
	if err != nil {
		t.Fatalf("Inspect() = %v", err)
	}
	if inspection.Status != StatusDrifted {
		t.Fatalf("status = %q, want %q", inspection.Status, StatusDrifted)
	}
}

func TestInspectRejectsEmptyPathSpecificRecovery(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "pi-worker")
	writeFile(t, filepath.Join(target, "tool.txt"), "tool")
	writeFile(t, filepath.Join(target, IdentityFile), IdentityContent)
	writeFile(t, filepath.Join(target, "SKILL.md"), "---\nname: pi-worker\n---\n")
	receipt := Receipt{
		SchemaVersion: SchemaVersion, InstallerVersion: "1", SkillsVersion: PinnedSkillsVersion, Outcome: OutcomeBlocked,
		Targets: []Target{{Path: target, Kind: targetKindCanonical, Files: []FileHash{
			{Path: "tool.txt", SHA256: hashString("tool")},
			{Path: IdentityFile, SHA256: hashString(IdentityContent)},
			{Path: "SKILL.md", SHA256: hashString("---\nname: pi-worker\n---\n")},
		}}},
		AffectedTargets: []AffectedTarget{{Path: target, State: AffectedUnmanaged, Recovery: []string{}}},
		Recovery:        []string{"npx --yes skills@" + PinnedSkillsVersion + " remove pi-worker -g -y", SafeRecoveryCommand},
	}
	inspection, err := Inspect(writeReceiptFromReceipt(t, root, receipt))
	if err != nil {
		t.Fatalf("Inspect() = %v", err)
	}
	if len(inspection.Recovery) != 0 {
		t.Fatalf("recovery = %v, want empty", inspection.Recovery)
	}
}

func TestLoadRejectsBackslashReceiptPaths(t *testing.T) {
	root := t.TempDir()
	receipt := validReceipt(filepath.Join(root, "target"), OutcomeInstalled)
	receipt.Targets[0].Files[0].Path = `dir\\file.txt`
	if _, err := Load(writeReceiptFromReceipt(t, root, receipt)); err == nil {
		t.Fatal("Load() = nil, want backslash path error")
	}
}

func TestPiWorkerFrontMatterRequiresStrictIdentityMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want bool
	}{
		{"malformed", "---\nnot metadata\nname: pi-worker\n---\n", false},
		{"duplicate name", "---\nname: pi-worker\nname: pi-worker\n---\n", false},
		{"valid extra metadata", "---\nname: pi-worker\ndescription: a skill\n# comment\n---\n", true},
		{"missing closing delimiter", "---\nname: pi-worker\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasPiWorkerFrontMatter([]byte(tc.data)); got != tc.want {
				t.Fatalf("hasPiWorkerFrontMatter() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInspectFailedAndSkippedExposeOnlyExactSafeRecovery(t *testing.T) {
	for _, outcome := range []Outcome{OutcomeFailed, OutcomeSkipped} {
		for _, recovery := range [][]string{{SafeRecoveryCommand}, {"arbitrary"}, {SafeRecoveryCommand, "arbitrary"}} {
			t.Run(string(outcome)+"/"+strings.Join(recovery, "+"), func(t *testing.T) {
				receipt := Receipt{SchemaVersion: SchemaVersion, InstallerVersion: "1", SkillsVersion: "1", Outcome: outcome, Recovery: recovery}
				inspection, err := Inspect(writeReceiptFromReceipt(t, t.TempDir(), receipt))
				if err != nil {
					t.Fatalf("Inspect() = %v", err)
				}
				want := []string{}
				if len(recovery) == 1 && recovery[0] == SafeRecoveryCommand {
					want = []string{SafeRecoveryCommand}
				}
				if !equalStringSlice(inspection.Recovery, want) {
					t.Fatalf("recovery = %v, want %v", inspection.Recovery, want)
				}
			})
		}
	}
}

func TestInspectSymlinkDestinationDrift(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip symlink test on windows")
	}
	root := t.TempDir()
	target := filepath.Join(root, "target")
	writeFile(t, filepath.Join(target, "first.txt"), "one")
	writeFile(t, filepath.Join(target, "second.txt"), "two")
	if err := os.Symlink("first.txt", filepath.Join(target, "link.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	receipt := Receipt{
		SchemaVersion:    SchemaVersion,
		InstallerVersion: "1",
		SkillsVersion:    "1",
		Outcome:          OutcomeInstalled,
		Targets: []Target{{
			Path:  target,
			Kind:  targetKindSymlink,
			Files: []FileHash{{Path: "link.txt", SHA256: hashString("first.txt")}},
		}},
	}
	path := writeReceiptFromReceipt(t, root, receipt)
	if _, err := Inspect(path); err != nil {
		t.Fatalf("Inspect() initial = %v", err)
	}
	if err := os.Remove(filepath.Join(target, "link.txt")); err != nil {
		t.Fatalf("remove symlink: %v", err)
	}
	if err := os.Symlink("second.txt", filepath.Join(target, "link.txt")); err != nil {
		t.Fatalf("rewrite symlink: %v", err)
	}
	inspection, err := Inspect(path)
	if err != nil {
		t.Fatalf("Inspect() changed = %v", err)
	}
	if inspection.Status != StatusDrifted {
		t.Fatalf("status = %q, want %q", inspection.Status, StatusDrifted)
	}
}

func TestInspectRejectsSymlinkedTargetRoots(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip symlink test on windows")
	}

	root := t.TempDir()
	for _, kind := range []string{targetKindCanonical, targetKindCopy} {
		t.Run(kind, func(t *testing.T) {
			realTarget := filepath.Join(root, kind+"-real")
			symlinkTarget := filepath.Join(root, kind+"-symlink")
			writeFile(t, filepath.Join(realTarget, "tool.txt"), "same bytes")
			if err := os.Symlink(realTarget, symlinkTarget); err != nil {
				t.Fatalf("symlink: %v", err)
			}

			receipt := Receipt{
				SchemaVersion:    SchemaVersion,
				InstallerVersion: "1",
				SkillsVersion:    "1",
				Outcome:          OutcomeInstalled,
				Targets: []Target{{
					Path:  symlinkTarget,
					Kind:  kind,
					Files: []FileHash{{Path: "tool.txt", SHA256: hashString("same bytes")}},
				}},
			}
			inspection, err := Inspect(writeReceiptFromReceipt(t, root, receipt))
			if err != nil {
				t.Fatalf("Inspect() = %v, want nil", err)
			}
			if inspection.Status != StatusDrifted {
				t.Fatalf("status = %q, want %q", inspection.Status, StatusDrifted)
			}
		})
	}
}

func TestInspectRejectsSymlinkedManagedFilesWithIdenticalBytes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip symlink test on windows")
	}

	root := t.TempDir()
	for _, kind := range []string{targetKindCanonical, targetKindCopy} {
		t.Run(kind, func(t *testing.T) {
			target := filepath.Join(root, kind)
			source := filepath.Join(root, kind+"-source.txt")
			writeFile(t, source, "same bytes")
			if err := os.MkdirAll(target, 0o700); err != nil {
				t.Fatalf("mkdir target: %v", err)
			}
			if err := os.Symlink(source, filepath.Join(target, "tool.txt")); err != nil {
				t.Fatalf("symlink: %v", err)
			}

			receipt := Receipt{
				SchemaVersion:    SchemaVersion,
				InstallerVersion: "1",
				SkillsVersion:    "1",
				Outcome:          OutcomeInstalled,
				Targets: []Target{{
					Path:  target,
					Kind:  kind,
					Files: []FileHash{{Path: "tool.txt", SHA256: hashString("same bytes")}},
				}},
			}
			inspection, err := Inspect(writeReceiptFromReceipt(t, root, receipt))
			if err != nil {
				t.Fatalf("Inspect() = %v, want nil", err)
			}
			if inspection.Status != StatusDrifted {
				t.Fatalf("status = %q, want %q", inspection.Status, StatusDrifted)
			}
		})
	}
}

func TestInspectRejectsDirectoryAndFIFOManagedFiles(t *testing.T) {
	for _, kind := range []string{targetKindCanonical, targetKindCopy} {
		t.Run(kind+" directory", func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "target")
			if err := os.MkdirAll(filepath.Join(target, "tool.txt"), 0o700); err != nil {
				t.Fatalf("mkdir managed directory: %v", err)
			}
			receipt := Receipt{
				SchemaVersion:    SchemaVersion,
				InstallerVersion: "1",
				SkillsVersion:    "1",
				Outcome:          OutcomeInstalled,
				Targets: []Target{{
					Path:  target,
					Kind:  kind,
					Files: []FileHash{{Path: "tool.txt", SHA256: hashString("same bytes")}},
				}},
			}
			inspection, err := Inspect(writeReceiptFromReceipt(t, root, receipt))
			if err != nil {
				t.Fatalf("Inspect() = %v, want nil", err)
			}
			if inspection.Status != StatusDrifted {
				t.Fatalf("status = %q, want %q", inspection.Status, StatusDrifted)
			}
		})

		t.Run(kind+" FIFO", func(t *testing.T) {
			if runtime.GOOS == "windows" {
				t.Skip("FIFO is not supported on windows")
			}
			if _, err := exec.LookPath("mkfifo"); err != nil {
				t.Skip("mkfifo is unavailable")
			}
			root := t.TempDir()
			target := filepath.Join(root, "target")
			if err := os.MkdirAll(target, 0o700); err != nil {
				t.Fatalf("mkdir target: %v", err)
			}
			fifo := filepath.Join(target, "tool.txt")
			if err := exec.Command("mkfifo", fifo).Run(); err != nil {
				t.Fatalf("mkfifo: %v", err)
			}
			receipt := Receipt{
				SchemaVersion:    SchemaVersion,
				InstallerVersion: "1",
				SkillsVersion:    "1",
				Outcome:          OutcomeInstalled,
				Targets: []Target{{
					Path:  target,
					Kind:  kind,
					Files: []FileHash{{Path: "tool.txt", SHA256: hashString("same bytes")}},
				}},
			}
			inspection, err := Inspect(writeReceiptFromReceipt(t, root, receipt))
			if err != nil {
				t.Fatalf("Inspect() = %v, want nil", err)
			}
			if inspection.Status != StatusDrifted {
				t.Fatalf("status = %q, want %q", inspection.Status, StatusDrifted)
			}
		})
	}
}

func TestInspectBlockedGlobalRecoveryPolicy(t *testing.T) {
	newReceipt := func(t *testing.T, identity []byte, addDrifted bool) string {
		t.Helper()
		root := t.TempDir()
		managed := filepath.Join(root, "pi-worker")
		writeFile(t, filepath.Join(managed, "tool.txt"), "ok")

		req := Receipt{
			SchemaVersion:    SchemaVersion,
			InstallerVersion: "1",
			SkillsVersion:    "1",
			Outcome:          OutcomeBlocked,
			Targets: []Target{{
				Path: managed,
				Kind: targetKindCanonical,
				Files: []FileHash{
					{Path: "tool.txt", SHA256: hashString("ok")},
					{Path: IdentityFile, SHA256: hashString(IdentityContent)},
					{Path: "SKILL.md", SHA256: hashString("---\nname: pi-worker\n---\n")},
				},
			}},
			AffectedTargets: []AffectedTarget{{Path: managed, State: AffectedUnmanaged, Recovery: []string{"Inspect and back up " + managed + " before retrying."}}},
			Recovery:        []string{"npx --yes skills@1.5.22 remove pi-worker -g -y", "npm install -g --foreground-scripts pi-worker"},
		}
		if addDrifted {
			req.AffectedTargets = append(req.AffectedTargets, AffectedTarget{Path: filepath.Join(root, "drifted"), State: AffectedDrifted, Recovery: []string{"backup drifted"}})
		}
		if identity != nil {
			if err := os.WriteFile(filepath.Join(managed, IdentityFile), identity, 0o600); err != nil {
				t.Fatalf("write identity: %v", err)
			}
			writeFile(t, filepath.Join(managed, "SKILL.md"), "---\nname: pi-worker\n---\n")
		}
		return writeReceiptFromReceipt(t, root, req)
	}

	t.Run("safe unmanaged path keeps global recovery", func(t *testing.T) {
		inspection, err := Inspect(newReceipt(t, []byte(IdentityContent), false))
		if err != nil {
			t.Fatalf("Inspect() = %v", err)
		}
		if inspection.Status != StatusBlocked {
			t.Fatalf("status = %q, want %q", inspection.Status, StatusBlocked)
		}
		wantRecovery := []string{"npx --yes skills@1.5.22 remove pi-worker -g -y", "npm install -g --foreground-scripts pi-worker"}
		if !equalStringSlice(inspection.Recovery, wantRecovery) {
			t.Fatalf("run-level recovery = %v, want %v", inspection.Recovery, wantRecovery)
		}
		if len(inspection.AffectedTargets) != 1 || len(inspection.AffectedTargets[0].Recovery) != 1 || inspection.AffectedTargets[0].Recovery[0] != "Inspect and back up "+filepath.Join(filepath.Dir(inspection.AffectedTargets[0].Path), "pi-worker")+" before retrying." {
			t.Fatalf("path-specific recovery = %+v", inspection.AffectedTargets)
		}
	})
	t.Run("markerless unmanaged path omits global", func(t *testing.T) {
		inspection, err := Inspect(newReceipt(t, nil, false))
		if err != nil {
			t.Fatalf("Inspect() = %v", err)
		}
		if containsValue(inspection.Recovery, "global remove all") {
			t.Fatalf("global recovery unexpectedly present: %v", inspection.Recovery)
		}
		if !equalStringSlice(inspection.Recovery, []string{SafeRecoveryCommand}) {
			t.Fatalf("missing target recovery = %v, want safe recovery", inspection.Recovery)
		}
	})
	t.Run("changed marker omits global", func(t *testing.T) {
		inspection, err := Inspect(newReceipt(t, []byte("changed\n"), false))
		if err != nil {
			t.Fatalf("Inspect() = %v", err)
		}
		if containsValue(inspection.Recovery, "global remove all") {
			t.Fatalf("global recovery unexpectedly present: %v", inspection.Recovery)
		}
	})
	t.Run("drifted affected path not in targets omits global", func(t *testing.T) {
		inspection, err := Inspect(newReceipt(t, []byte(IdentityContent), true))
		if err != nil {
			t.Fatalf("Inspect() = %v", err)
		}
		if containsValue(inspection.Recovery, "global remove all") {
			t.Fatalf("global recovery unexpectedly present: %v", inspection.Recovery)
		}
	})
}

func TestInspectPreservesMixedAffectedTargets(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "pi-worker")
	drifted := filepath.Join(root, "drifted")
	writeFile(t, filepath.Join(target, "tool.txt"), "one")
	writeFile(t, filepath.Join(drifted, "tool.txt"), "two")
	if err := os.WriteFile(filepath.Join(target, IdentityFile), []byte(IdentityContent), 0o600); err != nil {
		t.Fatalf("write identity: %v", err)
	}

	receipt := Receipt{
		SchemaVersion:    SchemaVersion,
		InstallerVersion: "1",
		SkillsVersion:    "1",
		Outcome:          OutcomeBlocked,
		Targets: []Target{{
			Path:  target,
			Kind:  targetKindCanonical,
			Files: []FileHash{{Path: "tool.txt", SHA256: hashString("one")}},
		}, {
			Path:  drifted,
			Kind:  targetKindCanonical,
			Files: []FileHash{{Path: "tool.txt", SHA256: hashString("two")}},
		}},
		AffectedTargets: []AffectedTarget{
			{Path: drifted, State: AffectedDrifted, Recovery: []string{"drifted-backup"}},
			{Path: filepath.Join(root, "other"), State: AffectedConflicting, Recovery: []string{"conflict-backup"}},
			{Path: target, State: AffectedUnmanaged, Recovery: []string{"managed-backup"}},
		},
		Recovery: []string{"global remove all"},
	}
	inspection, err := Inspect(writeReceiptFromReceipt(t, root, receipt))
	if err != nil {
		t.Fatalf("Inspect() = %v", err)
	}
	if len(inspection.AffectedTargets) != len(receipt.AffectedTargets) {
		t.Fatalf("affected len = %d, want %d", len(inspection.AffectedTargets), len(receipt.AffectedTargets))
	}
	expected := []AffectedTarget{
		{Path: drifted, State: AffectedDrifted},
		{Path: filepath.Join(root, "other"), State: AffectedConflicting},
		{Path: target, State: AffectedUnmanaged},
	}
	for i := range expected {
		if inspection.AffectedTargets[i].Path != expected[i].Path || inspection.AffectedTargets[i].State != expected[i].State {
			t.Fatalf("affected[%d] = %+v, want %+v", i, inspection.AffectedTargets[i], expected[i])
		}
	}
	if containsValue(inspection.Recovery, "global remove all") {
		t.Fatalf("global recovery should be omitted when conflicting/drifted exist: %v", inspection.Recovery)
	}
}

func TestInspectMissingReceipt(t *testing.T) {
	if _, err := Inspect(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("Inspect(missing path) = nil, want error")
	}
}

func TestIdentityMarkerAndGitAttributesFile(t *testing.T) {
	identity, err := os.ReadFile(filepath.Join("..", "..", "skills", "pi-worker", IdentityFile))
	if err != nil {
		t.Fatalf("read identity marker: %v", err)
	}
	if !bytes.Equal(identity, []byte(IdentityContent)) {
		t.Fatalf("identity marker bytes = %q, want %q", identity, []byte(IdentityContent))
	}
	if !bytes.HasSuffix(identity, []byte("\n")) {
		t.Fatalf("identity marker missing terminal newline: %q", identity)
	}
	if bytes.Contains(identity, []byte("\r")) {
		t.Fatalf("identity marker contains CR: %q", identity)
	}

	attributes, err := os.ReadFile(filepath.Join("..", "..", ".gitattributes"))
	if err != nil {
		t.Fatalf("read .gitattributes: %v", err)
	}
	if string(attributes) != "skills/pi-worker/PI_WORKER_IDENTITY -text\n" {
		t.Fatalf(".gitattributes = %q", string(attributes))
	}
}

func TestLoadRejectsSymlinkSpecialAndOversizedReceipts(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Run("symlink", func(t *testing.T) {
			root := t.TempDir()
			realPath := writeReceiptFromReceipt(t, root, validReceipt(filepath.Join(root, "target"), OutcomeInstalled))
			linkPath := filepath.Join(root, "receipt-link")
			if err := os.Symlink(realPath, linkPath); err != nil {
				t.Fatalf("symlink: %v", err)
			}
			if _, err := Load(linkPath); err == nil {
				t.Fatal("Load(symlink) = nil, want error")
			}
		})
	}

	t.Run("directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "receipt-dir")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if _, err := Load(path); err == nil {
			t.Fatal("Load(directory) = nil, want error")
		}
	})

	if runtime.GOOS != "windows" {
		t.Run("FIFO does not block", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "receipt-fifo")
			if err := exec.Command("mkfifo", path).Run(); err != nil {
				t.Fatalf("mkfifo: %v", err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := Load(path)
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("Load(FIFO) = nil, want error")
				}
			case <-time.After(time.Second):
				t.Fatal("Load(FIFO) blocked")
			}
		})
	}

	t.Run("oversized", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "oversized")
		if err := os.WriteFile(path, make([]byte, maxReceiptBytes+1), 0o600); err != nil {
			t.Fatalf("write oversized receipt: %v", err)
		}
		if _, err := Load(path); err == nil {
			t.Fatal("Load(oversized) = nil, want error")
		}
	})
}

func TestLoadRejectsNULInEveryReceiptStringField(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	mutations := []struct {
		name   string
		mutate func(*Receipt)
	}{
		{"installerVersion", func(r *Receipt) { r.InstallerVersion = "1\x00" }},

		{"skillsVersion", func(r *Receipt) { r.SkillsVersion = "1\x00" }},

		{"outcome", func(r *Receipt) { r.Outcome = Outcome("installed\x00") }},

		{"recovery", func(r *Receipt) { r.Recovery = []string{"recover\x00"} }},

		{"target path", func(r *Receipt) { r.Targets[0].Path = target + "\x00" }},

		{"target kind", func(r *Receipt) { r.Targets[0].Kind = "canonical\x00" }},

		{"file path", func(r *Receipt) { r.Targets[0].Files[0].Path = "file\x00" }},

		{"file sha256", func(r *Receipt) { r.Targets[0].Files[0].SHA256 = strings.Repeat("0", 63) + "\x00" }},

		{"affected path", func(r *Receipt) {
			r.Outcome = OutcomeBlocked
			r.AffectedTargets = []AffectedTarget{{Path: target + "\x00", State: AffectedDrifted, Recovery: []string{}}}
		}},

		{"affected state", func(r *Receipt) {
			r.Outcome = OutcomeBlocked
			r.AffectedTargets = []AffectedTarget{{Path: filepath.Join(root, "affected"), State: AffectedState("drifted\x00"), Recovery: []string{}}}
		}},

		{"affected recovery", func(r *Receipt) {
			r.Outcome = OutcomeBlocked
			r.AffectedTargets = []AffectedTarget{{Path: filepath.Join(root, "affected"), State: AffectedDrifted, Recovery: []string{"backup\x00"}}}
		}},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			receipt := validReceipt(target, OutcomeInstalled)
			tc.mutate(&receipt)
			if _, err := Load(writeReceiptFromReceipt(t, root, receipt)); err == nil {
				t.Fatal("Load() = nil, want NUL validation error")
			}
		})
	}
}

func TestLoadRejectsMalformedRequiredValues(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	cases := []struct {
		name   string
		mutate func(*Receipt)
	}{
		{name: "installerVersion", mutate: func(r *Receipt) { r.InstallerVersion = "" }},
		{name: "skillsVersion", mutate: func(r *Receipt) { r.SkillsVersion = "" }},
		{name: "targets null", mutate: func(r *Receipt) { r.Targets = nil }},
		{name: "affectedTargets null", mutate: func(r *Receipt) { r.AffectedTargets = nil }},
		{name: "recovery null", mutate: func(r *Receipt) { r.Recovery = nil }},
		{name: "target files null", mutate: func(r *Receipt) { r.Targets[0].Files = nil }},
		{name: "affected recovery null", mutate: func(r *Receipt) {
			r.Outcome = OutcomeBlocked
			r.AffectedTargets = []AffectedTarget{{Path: filepath.Join(root, "affected"), State: AffectedDrifted, Recovery: nil}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			receipt := validReceipt(target, OutcomeInstalled)
			tc.mutate(&receipt)
			path := writeReceiptBytes(t, root, mustMarshalReceipt(t, receipt))
			if _, err := Load(path); err == nil {
				t.Fatal("Load() = nil, want error")
			}
		})
	}
}

func TestInspectSuppressesUnsafeOrArbitraryRecovery(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "pi-worker")
	writeFile(t, filepath.Join(target, IdentityFile), IdentityContent)
	writeFile(t, filepath.Join(target, "SKILL.md"), "---\nname: pi-worker\n---\n")
	receipt := Receipt{
		SchemaVersion: SchemaVersion, InstallerVersion: "1", SkillsVersion: "1", Outcome: OutcomeBlocked,
		Targets: []Target{{Path: target, Kind: targetKindCanonical, Files: []FileHash{
			{Path: IdentityFile, SHA256: hashString(IdentityContent)},
			{Path: "SKILL.md", SHA256: hashString("---\nname: pi-worker\n---\n")},
			{Path: "missing", SHA256: strings.Repeat("0", 64)},
		}}},
		AffectedTargets: []AffectedTarget{{Path: target, State: AffectedUnmanaged, Recovery: []string{}}},
		Recovery:        []string{"arbitrary"},
	}
	inspection, err := Inspect(writeReceiptFromReceipt(t, root, receipt))
	if err != nil {
		t.Fatalf("Inspect() = %v", err)
	}
	if !equalStringSlice(inspection.Recovery, []string{SafeRecoveryCommand}) {
		t.Fatalf("recovery = %v, want safe recovery", inspection.Recovery)
	}
}

func TestInspectRequiresIdentityAndSkillFilesForUnmanagedRecovery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks are not consistently available on windows")
	}
	for _, tc := range []struct {
		name      string
		identity  bool
		skillLink bool
		idLink    bool
		skillData string
	}{
		{name: "marker only", identity: true},
		{name: "symlinked marker", identity: true, idLink: true},
		{name: "symlinked SKILL", identity: true, skillLink: true},
		{name: "unclosed SKILL front matter", identity: true, skillData: "---\nname: pi-worker\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "pi-worker")
			if err := os.MkdirAll(target, 0o700); err != nil {
				t.Fatalf("mkdir target: %v", err)
			}
			if tc.identity {
				identity := filepath.Join(root, "identity")
				writeFile(t, identity, IdentityContent)
				if tc.idLink {
					if err := os.Symlink(identity, filepath.Join(target, IdentityFile)); err != nil {
						t.Fatalf("identity symlink: %v", err)
					}
				} else {
					writeFile(t, filepath.Join(target, IdentityFile), IdentityContent)
				}
			}
			skill := filepath.Join(root, "skill")
			skillData := tc.skillData
			if skillData == "" {
				skillData = "---\nname: pi-worker\n---\n"
			}
			writeFile(t, skill, skillData)
			if tc.skillLink {
				if err := os.Symlink(skill, filepath.Join(target, "SKILL.md")); err != nil {
					t.Fatalf("SKILL symlink: %v", err)
				}
			} else if tc.name != "marker only" {
				writeFile(t, filepath.Join(target, "SKILL.md"), skillData)
			}
			inspection, err := Inspect(writeReceiptForRecoveryTest(t, root, target, AffectedUnmanaged, ""))
			if err != nil {
				t.Fatalf("Inspect() = %v", err)
			}
			if len(inspection.Recovery) != 0 {
				t.Fatalf("recovery = %v, want empty", inspection.Recovery)
			}
		})
	}
}

func TestInspectExposesRecoveryForValidUnmanagedAndDriftedTargets(t *testing.T) {
	t.Run("unmanaged", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "pi-worker")
		writeFile(t, filepath.Join(target, "tool.txt"), "expected")
		writeFile(t, filepath.Join(target, IdentityFile), IdentityContent)
		writeFile(t, filepath.Join(target, "SKILL.md"), "---\nname: pi-worker\n---\n")
		inspection, err := Inspect(writeReceiptForRecoveryTest(t, root, target, AffectedUnmanaged, target))
		if err != nil {
			t.Fatalf("Inspect() = %v", err)
		}
		assertGlobalRecoveryOrder(t, inspection.Recovery)
	})

	t.Run("drifted", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "pi-worker")
		writeFile(t, filepath.Join(target, "tool.txt"), "actual")
		inspection, err := Inspect(writeReceiptForRecoveryTest(t, root, target, AffectedDrifted, target))
		if err != nil {
			t.Fatalf("Inspect() = %v", err)
		}
		assertGlobalRecoveryOrder(t, inspection.Recovery)
	})

	t.Run("drifted path absent from Targets", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "pi-worker")
		inspection, err := Inspect(writeReceiptForRecoveryTest(t, root, target, AffectedDrifted, ""))
		if err != nil {
			t.Fatalf("Inspect() = %v", err)
		}
		if len(inspection.Recovery) != 0 {
			t.Fatalf("recovery = %v, want empty", inspection.Recovery)
		}
	})

	t.Run("observed drift absent from affected targets", func(t *testing.T) {
		root := t.TempDir()
		drifted := filepath.Join(root, "drifted")
		unmanaged := filepath.Join(root, "pi-worker")
		writeFile(t, filepath.Join(drifted, "tool.txt"), "actual")
		writeFile(t, filepath.Join(unmanaged, IdentityFile), IdentityContent)
		writeFile(t, filepath.Join(unmanaged, "SKILL.md"), "---\nname: pi-worker\n---\n")
		receipt := Receipt{
			SchemaVersion: SchemaVersion, InstallerVersion: "1", SkillsVersion: "1", Outcome: OutcomeBlocked,
			Targets:         []Target{{Path: drifted, Kind: targetKindCanonical, Files: []FileHash{{Path: "tool.txt", SHA256: hashString("expected")}}}},
			AffectedTargets: []AffectedTarget{{Path: unmanaged, State: AffectedUnmanaged, Recovery: []string{}}},
			Recovery:        []string{"npx --yes skills@1.5.22 remove pi-worker -g -y", "npm install -g --foreground-scripts pi-worker"},
		}
		inspection, err := Inspect(writeReceiptFromReceipt(t, root, receipt))
		if err != nil {
			t.Fatalf("Inspect() = %v", err)
		}
		if len(inspection.Recovery) != 0 {
			t.Fatalf("recovery = %v, want empty", inspection.Recovery)
		}
	})
}

func writeReceiptForRecoveryTest(t *testing.T, root, affected string, state AffectedState, targetPath string) string {
	t.Helper()
	receipt := Receipt{
		SchemaVersion: SchemaVersion, InstallerVersion: "1", SkillsVersion: "1", Outcome: OutcomeBlocked,
		AffectedTargets: []AffectedTarget{{Path: affected, State: state, Recovery: []string{"Inspect and back up " + affected + " before retrying."}}},
		Recovery:        []string{"npx --yes skills@1.5.22 remove pi-worker -g -y", "npm install -g --foreground-scripts pi-worker"},
	}
	if targetPath != "" {
		files := []FileHash{{Path: "tool.txt", SHA256: hashString("expected")}}
		if state == AffectedUnmanaged {
			files = append(files,
				FileHash{Path: IdentityFile, SHA256: hashString(IdentityContent)},
				FileHash{Path: "SKILL.md", SHA256: hashString("---\nname: pi-worker\n---\n")},
			)
		}
		receipt.Targets = []Target{{Path: targetPath, Kind: targetKindCanonical, Files: files}}
	}
	return writeReceiptFromReceipt(t, root, receipt)
}

func assertGlobalRecoveryOrder(t *testing.T, got []string) {
	t.Helper()
	want := []string{"npx --yes skills@1.5.22 remove pi-worker -g -y", "npm install -g --foreground-scripts pi-worker"}
	if !equalStringSlice(got, want) {
		t.Fatalf("recovery = %v, want %v", got, want)
	}
}

func mustMarshalJSON(value any) []byte {
	b, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return b
}

func mustMarshalReceipt(t *testing.T, receipt Receipt) []byte {
	t.Helper()
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("marshal receipt: %v", err)
	}
	return data
}

func writeReceiptFromReceipt(t *testing.T, dir string, receipt Receipt) string {
	t.Helper()
	if receipt.Targets == nil {
		receipt.Targets = []Target{}
	}
	if receipt.AffectedTargets == nil {
		receipt.AffectedTargets = []AffectedTarget{}
	}
	if receipt.Recovery == nil {
		receipt.Recovery = []string{}
	}
	for i := range receipt.AffectedTargets {
		if receipt.AffectedTargets[i].Recovery == nil {
			receipt.AffectedTargets[i].Recovery = []string{}
		}
	}
	path := filepath.Join(dir, "skill-install.json")
	if err := os.WriteFile(path, mustMarshalReceipt(t, receipt), 0o600); err != nil {
		t.Fatalf("write receipt %q: %v", path, err)
	}
	return path
}

func writeReceiptBytes(t *testing.T, dir string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, "skill-install.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write receipt %q: %v", path, err)
	}
	return path
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %q: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

func modifyReceipt(receipt Receipt, mutate func(*Receipt)) Receipt {
	mutate(&receipt)
	return receipt
}

func validReceipt(target string, outcome Outcome) Receipt {
	return Receipt{
		SchemaVersion:    SchemaVersion,
		InstallerVersion: "1",
		SkillsVersion:    "1",
		Outcome:          outcome,
		Targets:          []Target{{Path: target, Kind: targetKindCanonical, Files: []FileHash{{Path: "one.txt", SHA256: strings.Repeat("0", 64)}}}},
		AffectedTargets:  []AffectedTarget{},
		Recovery:         []string{},
	}
}

func hashString(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func equalStringSlice(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func containsValue(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

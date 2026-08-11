package skillinstall

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
	receipt := Receipt{
		SchemaVersion:    SchemaVersion,
		InstallerVersion: "1",
		SkillsVersion:    "1",
		Outcome:          OutcomeBlocked,
		Targets:          []Target{{Path: filepath.Join(t.TempDir(), "target"), Kind: targetKindCanonical}},
		AffectedTargets:  []AffectedTarget{{Path: filepath.Join(t.TempDir(), "other"), State: AffectedConflicting, Recovery: []string{"path backup"}}},
		Recovery:         []string{"global remove"},
	}
	if _, err := Load(writeReceiptFromReceipt(t, t.TempDir(), receipt)); err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
}

func TestInspectVerifiesAndClassifiesTargets(t *testing.T) {
	root := t.TempDir()
	targetA := filepath.Join(root, "zzz")
	targetB := filepath.Join(root, "aaa")
	writeFile(t, filepath.Join(targetA, "a.txt"), "one")
	writeFile(t, filepath.Join(targetB, "b.txt"), "two")

	receipt := Receipt{
		SchemaVersion:    SchemaVersion,
		InstallerVersion: "1",
		SkillsVersion:    "1",
		Outcome:          OutcomeInstalled,
		Targets: []Target{{
			Path:  targetA,
			Kind:  targetKindCanonical,
			Files: []FileHash{{Path: "a.txt", SHA256: hashString("one")}},
		}, {
			Path:  targetB,
			Kind:  targetKindCanonical,
			Files: []FileHash{{Path: "b.txt", SHA256: hashString("two")}},
		}},
	}
	inspection, err := Inspect(writeReceiptFromReceipt(t, root, receipt))
	if err != nil {
		t.Fatalf("Inspect() = %v, want nil", err)
	}
	if inspection.Status != StatusVerified {
		t.Fatalf("status = %q, want %q", inspection.Status, StatusVerified)
	}
	expected := []string{filepath.Clean(targetB), filepath.Clean(targetA)}
	if !equalStringSlice(inspection.VerifiedTargets, expected) {
		t.Fatalf("verified targets = %v, want %v", inspection.VerifiedTargets, expected)
	}

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
	})

	t.Run("drifted target", func(t *testing.T) {
		receipt := Receipt{
			SchemaVersion:    SchemaVersion,
			InstallerVersion: "1",
			SkillsVersion:    "1",
			Outcome:          OutcomeInstalled,
			Targets: []Target{{
				Path:  targetA,
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
				Path:  managed,
				Kind:  targetKindCanonical,
				Files: []FileHash{{Path: "tool.txt", SHA256: hashString("ok")}},
			}},
			AffectedTargets: []AffectedTarget{{Path: managed, State: AffectedUnmanaged, Recovery: []string{"backup managed"}}},
			Recovery:        []string{"global remove all"},
		}
		if addDrifted {
			req.AffectedTargets = append(req.AffectedTargets, AffectedTarget{Path: filepath.Join(root, "drifted"), State: AffectedDrifted, Recovery: []string{"backup drifted"}})
		}
		if identity != nil {
			if err := os.WriteFile(filepath.Join(managed, IdentityFile), identity, 0o600); err != nil {
				t.Fatalf("write identity: %v", err)
			}
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
		if len(inspection.Recovery) != 1 || inspection.Recovery[0] != "global remove all" {
			t.Fatalf("run-level recovery = %v, want only global remove", inspection.Recovery)
		}
		if len(inspection.AffectedTargets) != 1 || len(inspection.AffectedTargets[0].Recovery) != 1 || inspection.AffectedTargets[0].Recovery[0] != "backup managed" {
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
		if len(inspection.Recovery) != 0 {
			t.Fatalf("run-level recovery must stay empty when global recovery is unsafe: %v", inspection.Recovery)
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

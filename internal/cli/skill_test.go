package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pi-worker/internal/skillinstall"
)

func TestSkillReceiptPathOutputsPathForHumanAndJSON(t *testing.T) {
	receiptPath := filepath.Join(t.TempDir(), "skill-install.json")
	installSkillReceiptPath(t, receiptPath)

	code, stdout, stderr := runCLI(t, []string{"skill", "receipt-path"}, "")
	if code != 0 || stdout != receiptPath+"\n" || stderr != "" {
		t.Fatalf("receipt-path = (%d, %q, %q)", code, stdout, stderr)
	}

	code, stdout, stderr = runCLI(t, []string{"skill", "receipt-path", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("receipt-path --json = (%d, %q)", code, stderr)
	}
	if strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
		t.Fatalf("stdout has multiple documents: %q", stdout)
	}
	var got struct {
		SchemaVersion int    `json:"schemaVersion"`
		ReceiptPath   string `json:"receiptPath"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode receipt-path JSON %q: %v", stdout, err)
	}
	if got.SchemaVersion != skillinstall.SchemaVersion || got.ReceiptPath != receiptPath {
		t.Fatalf("receipt-path output = %#v", got)
	}
}

func TestSkillReceiptPathRejectsDuplicateOrUnknownFlagsAndArguments(t *testing.T) {
	installSkillReceiptPath(t, filepath.Join(t.TempDir(), "skill-install.json"))

	for _, args := range [][]string{
		{"skill", "receipt-path", "--json", "--json"},
		{"skill", "receipt-path", "--bogus"},
		{"skill", "receipt-path", "extra"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, stdout, stderr := runCLI(t, args, "")
			if code != 2 || stdout != "" || !strings.Contains(stderr, "pi-worker:") {
				t.Fatalf("receipt-path %v = (%d, %q, %q)", args, code, stdout, stderr)
			}
		})
	}
}

func TestSkillReceiptPathRequiresAbsolutePath(t *testing.T) {
	installSkillReceiptPath(t, filepath.Join("relative", "skill-install.json"))
	for _, args := range [][]string{{"skill", "receipt-path"}, {"skill", "receipt-path", "--json"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, stdout, stderr := runCLI(t, args, "")
			if code != 9 || stdout != "" || !strings.Contains(stderr, "absolute") {
				t.Fatalf("receipt-path %v = (%d, %q, %q)", args, code, stdout, stderr)
			}
		})
	}
}

func TestSkillReceiptPathFailureFromPathResolverReturnsInternalError(t *testing.T) {
	errResolver := errors.New("os.UserConfigDir failure")
	installSkillReceiptPathFunc(t, func() (string, error) { return "", errResolver })

	code, stdout, stderr := runCLI(t, []string{"skill", "receipt-path"}, "")
	if code != 9 || stdout != "" || !strings.Contains(stderr, "determine skill receipt path") {
		t.Fatalf("receipt-path failure = (%d, %q, %q)", code, stdout, stderr)
	}
}

func TestSkillReceiptPathCancellationAfterResolutionDoesNotRenderResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "skill-install.json")
	entered := make(chan struct{})
	release := make(chan struct{})
	installSkillReceiptPathFunc(t, func() (string, error) {
		close(entered)
		<-release
		return path, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		code   int
		stdout string
		stderr string
	}
	done := make(chan result, 1)
	go func() {
		code, stdout, stderr := runCLIWithContext(t, ctx, []string{"skill", "receipt-path", "--json"}, "")
		done <- result{code: code, stdout: stdout, stderr: stderr}
	}()
	<-entered
	cancel()
	close(release)
	got := <-done
	if got.code != 8 || got.stdout != "" || !strings.Contains(got.stderr, "skill cancelled") {
		t.Fatalf("receipt-path cancelled after resolution = (%d, %q, %q)", got.code, got.stdout, got.stderr)
	}
}

func TestSkillStatusVerifiedOutputsExit0AndSortedTargets(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "zzz")
	second := filepath.Join(root, "aaa")
	writeFileForSkillTest(t, filepath.Join(first, "one.txt"), "one")
	writeFileForSkillTest(t, filepath.Join(second, "two.txt"), "two")

	receipt := skillinstall.Receipt{
		SchemaVersion:    skillinstall.SchemaVersion,
		InstallerVersion: "1",
		SkillsVersion:    "1",
		Outcome:          skillinstall.OutcomeInstalled,
		Targets: []skillinstall.Target{{
			Path:  first,
			Kind:  "canonical",
			Files: []skillinstall.FileHash{{Path: "one.txt", SHA256: hashString(t, "one")}},
		}, {
			Path:  second,
			Kind:  "canonical",
			Files: []skillinstall.FileHash{{Path: "two.txt", SHA256: hashString(t, "two")}},
		}},
	}
	path := filepath.Join(root, "skill-install.json")
	writeReceiptForSkillTest(t, path, receipt)
	installSkillReceiptPath(t, path)

	code, stdout, stderr := runCLI(t, []string{"skill", "status", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("status verified json = (%d, %q, %q)", code, stdout, stderr)
	}
	var got skillinstall.Inspection
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode status JSON %q: %v", stdout, err)
	}
	if got.Status != skillinstall.StatusVerified {
		t.Fatalf("status = %s, want %s", got.Status, skillinstall.StatusVerified)
	}
	if got.VerifiedTargets[0] != filepath.Clean(second) || got.VerifiedTargets[1] != filepath.Clean(first) {
		t.Fatalf("verified targets = %v", got.VerifiedTargets)
	}

	code, stdout, stderr = runCLI(t, []string{"skill", "status"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("status verified human = (%d, %q, %q)", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "status: verified") || !strings.Contains(stdout, "verified-targets:") {
		t.Fatalf("human status = %q", stdout)
	}
}

func TestSkillStatusMissingReceiptPathIsNotReady(t *testing.T) {
	path := filepath.Join(t.TempDir(), "skill-install.json")
	installSkillReceiptPath(t, path)

	code, stdout, stderr := runCLI(t, []string{"skill", "status", "--json"}, "")
	if code != 3 || stderr != "" {
		t.Fatalf("status missing = (%d, %q, %q)", code, stdout, stderr)
	}
	if strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
		t.Fatalf("status json has multiple documents: %q", stdout)
	}
	var got skillinstall.Inspection
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode missing receipt JSON %q: %v", stdout, err)
	}
	if got.Status != skillinstall.StatusMissing || got.ReceiptPath != path {
		t.Fatalf("missing-receipt output = %#v", got)
	}
}

func TestSkillStatusMissingOrDriftedTargetsReturnExit3(t *testing.T) {
	t.Run("missing target", func(t *testing.T) {
		root := t.TempDir()
		receipt := skillinstall.Receipt{
			SchemaVersion:    skillinstall.SchemaVersion,
			InstallerVersion: "1",
			SkillsVersion:    "1",
			Outcome:          skillinstall.OutcomeInstalled,
			Targets: []skillinstall.Target{{
				Path:  filepath.Join(root, "missing"),
				Kind:  "canonical",
				Files: []skillinstall.FileHash{{Path: "one.txt", SHA256: hashString(t, "one")}},
			}},
		}
		path := filepath.Join(root, "skill-install.json")
		writeReceiptForSkillTest(t, path, receipt)
		installSkillReceiptPath(t, path)

		code, stdout, stderr := runCLI(t, []string{"skill", "status"}, "")
		if code != 3 || stderr != "" {
			t.Fatalf("status missing target = (%d, %q, %q)", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "status: missing") {
			t.Fatalf("status output = %q", stdout)
		}
	})

	t.Run("hash drift", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target")
		writeFileForSkillTest(t, filepath.Join(target, "one.txt"), "one")
		receipt := skillinstall.Receipt{
			SchemaVersion:    skillinstall.SchemaVersion,
			InstallerVersion: "1",
			SkillsVersion:    "1",
			Outcome:          skillinstall.OutcomeInstalled,
			Targets: []skillinstall.Target{{
				Path:  target,
				Kind:  "canonical",
				Files: []skillinstall.FileHash{{Path: "one.txt", SHA256: hashString(t, "changed")}},
			}},
		}
		path := filepath.Join(root, "skill-install.json")
		writeReceiptForSkillTest(t, path, receipt)
		installSkillReceiptPath(t, path)

		code, stdout, stderr := runCLI(t, []string{"skill", "status", "--json"}, "")
		if code != 3 || stderr != "" {
			t.Fatalf("status drift = (%d, %q, %q)", code, stdout, stderr)
		}
		var got skillinstall.Inspection
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("decode drift JSON %q: %v", stdout, err)
		}
		if got.Status != skillinstall.StatusDrifted {
			t.Fatalf("status = %s, want %s", got.Status, skillinstall.StatusDrifted)
		}
	})
}

func TestSkillStatusBlockedUnmanagedIncludesPathSpecificRecovery(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "pi-worker")
	writeFileForSkillTest(t, filepath.Join(target, "tool.txt"), "tool")
	if err := os.WriteFile(filepath.Join(target, skillinstall.IdentityFile), []byte(skillinstall.IdentityContent), 0o600); err != nil {
		t.Fatalf("write identity: %v", err)
	}
	receipt := skillinstall.Receipt{
		SchemaVersion:    skillinstall.SchemaVersion,
		InstallerVersion: "1",
		SkillsVersion:    "1",
		Outcome:          skillinstall.OutcomeBlocked,
		Targets: []skillinstall.Target{{
			Path:  target,
			Kind:  "canonical",
			Files: []skillinstall.FileHash{{Path: "tool.txt", SHA256: hashString(t, "tool")}},
		}},
		AffectedTargets: []skillinstall.AffectedTarget{{
			Path:     target,
			State:    skillinstall.AffectedUnmanaged,
			Recovery: []string{"backup managed"},
		}},
		Recovery: []string{"global remove all"},
	}
	path := filepath.Join(root, "skill-install.json")
	writeReceiptForSkillTest(t, path, receipt)
	installSkillReceiptPath(t, path)

	code, stdout, stderr := runCLI(t, []string{"skill", "status"}, "")
	if code != 3 || stderr != "" {
		t.Fatalf("status unmanaged = (%d, %q, %q)", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "status: blocked") || !strings.Contains(stdout, target+" (unmanaged)") || !strings.Contains(stdout, "backup managed") {
		t.Fatalf("human status = %q", stdout)
	}
	if strings.Count(stdout, "backup managed") != 1 {
		t.Fatalf("path-specific recovery must be rendered once: %q", stdout)
	}

	code, stdout, stderr = runCLI(t, []string{"skill", "status", "--json"}, "")
	if code != 3 || stderr != "" {
		t.Fatalf("status unmanaged json = (%d, %q, %q)", code, stdout, stderr)
	}
	var got skillinstall.Inspection
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode JSON %q: %v", stdout, err)
	}
	if got.Status != skillinstall.StatusBlocked {
		t.Fatalf("status = %s", got.Status)
	}
	if !containsRecovery(got.Recovery, "global remove all") {
		t.Fatalf("recovery = %v", got.Recovery)
	}
}

func TestSkillStatusBlockedDriftedConflictingAndMixedTargetStates(t *testing.T) {
	root := t.TempDir()
	managed := filepath.Join(root, "pi-worker")
	drifted := filepath.Join(root, "drifted")
	foreign := filepath.Join(root, "foreign")

	writeFileForSkillTest(t, filepath.Join(managed, "tool.txt"), "managed")
	writeFileForSkillTest(t, filepath.Join(drifted, "tool.txt"), "drifted")
	if err := os.WriteFile(filepath.Join(managed, skillinstall.IdentityFile), []byte(skillinstall.IdentityContent), 0o600); err != nil {
		t.Fatalf("write identity: %v", err)
	}

	receipt := skillinstall.Receipt{
		SchemaVersion:    skillinstall.SchemaVersion,
		InstallerVersion: "1",
		SkillsVersion:    "1",
		Outcome:          skillinstall.OutcomeBlocked,
		Targets: []skillinstall.Target{{
			Path:  managed,
			Kind:  "canonical",
			Files: []skillinstall.FileHash{{Path: "tool.txt", SHA256: hashString(t, "managed")}},
		}, {
			Path:  drifted,
			Kind:  "canonical",
			Files: []skillinstall.FileHash{{Path: "tool.txt", SHA256: hashString(t, "drifted")}},
		}},
		AffectedTargets: []skillinstall.AffectedTarget{{
			Path:     managed,
			State:    skillinstall.AffectedUnmanaged,
			Recovery: []string{"backup managed"},
		}, {
			Path:     drifted,
			State:    skillinstall.AffectedDrifted,
			Recovery: []string{"backup drifted"},
		}, {
			Path:     foreign,
			State:    skillinstall.AffectedConflicting,
			Recovery: []string{"backup foreign"},
		}},
		Recovery: []string{"global remove all"},
	}
	path := filepath.Join(root, "skill-install.json")
	writeReceiptForSkillTest(t, path, receipt)
	installSkillReceiptPath(t, path)

	code, stdout, stderr := runCLI(t, []string{"skill", "status", "--json"}, "")
	if code != 3 || stderr != "" {
		t.Fatalf("status mixed json = (%d, %q, %q)", code, stdout, stderr)
	}
	if strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
		t.Fatalf("status mixed is not one document: %q", stdout)
	}
	var got skillinstall.Inspection
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode mixed JSON %q: %v", stdout, err)
	}
	if got.Status != skillinstall.StatusBlocked {
		t.Fatalf("status = %s", got.Status)
	}
	if len(got.AffectedTargets) != 3 {
		t.Fatalf("got affected = %v", got.AffectedTargets)
	}
	if got.AffectedTargets[0].State != skillinstall.AffectedDrifted || got.AffectedTargets[1].State != skillinstall.AffectedConflicting || got.AffectedTargets[2].State != skillinstall.AffectedUnmanaged {
		t.Fatalf("affected states = %v", got.AffectedTargets)
	}
	if containsRecovery(got.Recovery, "global remove all") {
		t.Fatalf("mixed affected paths should omit global recovery: %v", got.Recovery)
	}

	// Human output validates per-target recovery lines.
	code, stdout, stderr = runCLI(t, []string{"skill", "status"}, "")
	if code != 3 || stderr != "" {
		t.Fatalf("status mixed = (%d, %q, %q)", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "status: blocked") {
		t.Fatalf("human status = %q", stdout)
	}
	if !strings.Contains(stdout, managed+" (unmanaged)") || !strings.Contains(stdout, "backup managed") {
		t.Fatalf("human managed recovery = %q", stdout)
	}
	if !strings.Contains(stdout, drifted+" (drifted)") || !strings.Contains(stdout, "backup drifted") {
		t.Fatalf("human drifted recovery = %q", stdout)
	}
	if !strings.Contains(stdout, foreign+" (conflicting)") || !strings.Contains(stdout, "backup foreign") {
		t.Fatalf("human conflicting recovery = %q", stdout)
	}
}

func TestSkillStatusMarkerlessUnmanagedConflictingPathOmitsGlobalRecovery(t *testing.T) {
	root := t.TempDir()
	managed := filepath.Join(root, "pi-worker")
	writeFileForSkillTest(t, filepath.Join(managed, "tool.txt"), "tool")
	receipt := skillinstall.Receipt{
		SchemaVersion:    skillinstall.SchemaVersion,
		InstallerVersion: "1",
		SkillsVersion:    "1",
		Outcome:          skillinstall.OutcomeBlocked,
		Targets: []skillinstall.Target{{
			Path:  managed,
			Kind:  "canonical",
			Files: []skillinstall.FileHash{{Path: "tool.txt", SHA256: hashString(t, "tool")}},
		}},
		AffectedTargets: []skillinstall.AffectedTarget{{
			Path:     managed,
			State:    skillinstall.AffectedUnmanaged,
			Recovery: []string{"backup managed"},
		}},
		Recovery: []string{"global remove all"},
	}
	path := filepath.Join(root, "skill-install.json")
	writeReceiptForSkillTest(t, path, receipt)
	installSkillReceiptPath(t, path)

	code, stdout, stderr := runCLI(t, []string{"skill", "status", "--json"}, "")
	if code != 3 || stderr != "" {
		t.Fatalf("status markerless = (%d, %q, %q)", code, stdout, stderr)
	}
	var got skillinstall.Inspection
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode markerless JSON %q: %v", stdout, err)
	}
	if containsRecovery(got.Recovery, "global remove all") {
		t.Fatalf("global recovery unexpectedly exposed: %v", got.Recovery)
	}
}

func TestSkillStatusSkippedAndFailedOutcomesReturnReadyStyleExit3(t *testing.T) {
	for _, outcome := range []skillinstall.Outcome{skillinstall.OutcomeSkipped, skillinstall.OutcomeFailed} {
		t.Run(string(outcome), func(t *testing.T) {
			root := t.TempDir()
			receipt := skillinstall.Receipt{
				SchemaVersion:    skillinstall.SchemaVersion,
				InstallerVersion: "1",
				SkillsVersion:    "1",
				Outcome:          outcome,
			}
			path := filepath.Join(root, "skill-install.json")
			writeReceiptForSkillTest(t, path, receipt)
			installSkillReceiptPath(t, path)

			code, stdout, stderr := runCLI(t, []string{"skill", "status"}, "")
			if code != 3 || stderr != "" {
				t.Fatalf("status %s = (%d, %q, %q)", outcome, code, stdout, stderr)
			}
			if !strings.Contains(stdout, "status: "+string(outcome)) {
				t.Fatalf("status output = %q", stdout)
			}
		})
	}
}

func TestSkillStatusMalformedReceiptIsInternalFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "skill-install.json")
	if err := os.WriteFile(path, []byte(`{"schemaVersion":1,"installerVersion":"1","outcome":"installed"`), 0o600); err != nil {
		t.Fatalf("write malformed receipt: %v", err)
	}
	installSkillReceiptPath(t, path)

	code, stdout, stderr := runCLI(t, []string{"skill", "status", "--json"}, "")
	if code != 9 || stdout != "" || !strings.Contains(stderr, "inspect skill status") {
		t.Fatalf("malformed status = (%d, %q, %q)", code, stdout, stderr)
	}
}

func TestSkillStatusUsageAndCancellationHaveNoStdout(t *testing.T) {
	installSkillReceiptPath(t, filepath.Join(t.TempDir(), "skill-install.json"))

	code, stdout, stderr := runCLI(t, []string{"skill", "status", "--json", "extra"}, "")
	if code != 2 || stdout != "" || !strings.Contains(stderr, "pi-worker:") {
		t.Fatalf("status usage = (%d, %q, %q)", code, stdout, stderr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, stdout, stderr = runCLIWithContext(t, ctx, []string{"skill", "status", "--json"}, "")
	if code != 8 || stdout != "" || !strings.Contains(stderr, "skill cancelled") {
		t.Fatalf("status cancelled = (%d, %q, %q)", code, stdout, stderr)
	}
}

func TestSkillStatusCancellationAfterInspectionDoesNotRenderResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "skill-install.json")
	installSkillReceiptPath(t, path)
	entered := make(chan struct{})
	release := make(chan struct{})
	installSkillInspector(t, func(string) (skillinstall.Inspection, error) {
		close(entered)
		<-release
		return skillinstall.Inspection{
			SchemaVersion: skillinstall.SchemaVersion,
			ReceiptPath:   path,
			Status:        skillinstall.StatusVerified,
		}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		code   int
		stdout string
		stderr string
	}
	done := make(chan result, 1)
	go func() {
		code, stdout, stderr := runCLIWithContext(t, ctx, []string{"skill", "status", "--json"}, "")
		done <- result{code: code, stdout: stdout, stderr: stderr}
	}()
	<-entered
	cancel()
	close(release)
	got := <-done
	if got.code != 8 || got.stdout != "" || !strings.Contains(got.stderr, "skill cancelled") {
		t.Fatalf("status cancelled after inspection = (%d, %q, %q)", got.code, got.stdout, got.stderr)
	}
}

func installSkillReceiptPathFunc(t *testing.T, resolver func() (string, error)) {
	t.Helper()
	original := resolveSkillReceiptPath
	resolveSkillReceiptPath = resolver
	t.Cleanup(func() { resolveSkillReceiptPath = original })
}

func installSkillReceiptPath(t *testing.T, path string) {
	t.Helper()
	installSkillReceiptPathFunc(t, func() (string, error) { return path, nil })
}

func installSkillInspector(t *testing.T, inspect func(string) (skillinstall.Inspection, error)) {
	t.Helper()
	original := inspectSkillReceipt
	inspectSkillReceipt = inspect
	t.Cleanup(func() { inspectSkillReceipt = original })
}

func writeReceiptForSkillTest(t *testing.T, path string, receipt skillinstall.Receipt) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %q: %v", filepath.Dir(path), err)
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("marshal receipt: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write receipt: %v", err)
	}
}

func writeFileForSkillTest(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %q: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

func hashString(t *testing.T, value string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func containsRecovery(recovery []string, want string) bool {
	for _, value := range recovery {
		if value == want {
			return true
		}
	}
	return false
}

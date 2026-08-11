package skillinstall

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const SchemaVersion = 1

const IdentityFile = "PI_WORKER_IDENTITY"

const IdentityContent = "pi-worker-skill/v1\n"

const (
	targetKindCanonical = "canonical"
	targetKindSymlink   = "symlink"
	targetKindCopy      = "copy"
)

type Outcome string

const (
	OutcomeInstalled Outcome = "installed"
	OutcomeBlocked   Outcome = "blocked"
	OutcomeSkipped   Outcome = "skipped"
	OutcomeFailed    Outcome = "failed"
)

type AffectedState string

const (
	AffectedUnmanaged   AffectedState = "unmanaged"
	AffectedDrifted     AffectedState = "drifted"
	AffectedConflicting AffectedState = "conflicting"
)

type InspectionStatus string

const (
	StatusVerified InspectionStatus = "verified"
	StatusMissing  InspectionStatus = "missing"
	StatusBlocked  InspectionStatus = "blocked"
	StatusDrifted  InspectionStatus = "drifted"
	StatusSkipped  InspectionStatus = "skipped"
	StatusFailed   InspectionStatus = "failed"
)

type FileHash struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type Target struct {
	Path  string     `json:"path"`
	Kind  string     `json:"kind"`
	Files []FileHash `json:"files"`
}

type AffectedTarget struct {
	Path     string        `json:"path"`
	State    AffectedState `json:"state"`
	Recovery []string      `json:"recovery"`
}

type Receipt struct {
	SchemaVersion    int              `json:"schemaVersion"`
	InstallerVersion string           `json:"installerVersion"`
	SkillsVersion    string           `json:"skillsVersion"`
	Outcome          Outcome          `json:"outcome"`
	Targets          []Target         `json:"targets"`
	AffectedTargets  []AffectedTarget `json:"affectedTargets"`
	Recovery         []string         `json:"recovery"`
}

type Inspection struct {
	SchemaVersion   int              `json:"schemaVersion"`
	ReceiptPath     string           `json:"receiptPath"`
	Status          InspectionStatus `json:"status"`
	VerifiedTargets []string         `json:"verifiedTargets"`
	AffectedTargets []AffectedTarget `json:"affectedTargets"`
	Recovery        []string         `json:"recovery"`
}

// Load decodes a receipt file and validates it structurally.
func Load(path string) (Receipt, error) {
	f, err := os.Open(path)
	if err != nil {
		return Receipt{}, err
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var receipt Receipt
	if err := dec.Decode(&receipt); err != nil {
		return Receipt{}, fmt.Errorf("load receipt %s: %w", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return Receipt{}, fmt.Errorf("load receipt %s: trailing data after document", path)
		}
		return Receipt{}, fmt.Errorf("load receipt %s: %w", path, err)
	}
	if err := validateReceipt(receipt); err != nil {
		return Receipt{}, fmt.Errorf("validate receipt %s: %w", path, err)
	}
	return receipt, nil
}

func Inspect(path string) (Inspection, error) {
	receipt, err := Load(path)
	if err != nil {
		return Inspection{}, err
	}

	verifiedTargets, missingTargets, driftedTargets, err := inspectTargets(receipt.Targets)
	if err != nil {
		return Inspection{}, err
	}

	insp := Inspection{
		SchemaVersion:   SchemaVersion,
		ReceiptPath:     path,
		VerifiedTargets: append([]string{}, verifiedTargets...),
		AffectedTargets: cloneAffectedTargets(receipt.AffectedTargets),
		Status:          statusFromReceiptOutcomeAndTargets(receipt.Outcome, missingTargets, driftedTargets),
	}
	sort.Strings(insp.VerifiedTargets)

	recovery := collectPathSpecificRecovery(receipt.AffectedTargets)
	if shouldExposeGlobalRemove(receipt, missingTargets, driftedTargets) {
		recovery = append(recovery, receipt.Recovery...)
	}
	sort.Strings(recovery)
	insp.Recovery = recovery
	return insp, nil
}

func validateReceipt(r Receipt) error {
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schemaVersion %d: want %d", r.SchemaVersion, SchemaVersion)
	}
	if !isValidOutcome(r.Outcome) {
		return fmt.Errorf("invalid outcome %q", r.Outcome)
	}

	if r.Outcome == OutcomeInstalled && len(r.AffectedTargets) != 0 {
		return errors.New("installed outcome must not include affectedTargets")
	}
	if r.Outcome == OutcomeBlocked && len(r.AffectedTargets) == 0 {
		return errors.New("blocked outcome requires at least one affectedTarget")
	}

	if err := validateRecoveryCommands(r.Recovery, "recovery"); err != nil {
		return err
	}

	seenTargets := map[string]struct{}{}
	for _, t := range r.Targets {
		if t.Path == "" {
			return errors.New("target path is empty")
		}
		if !filepath.IsAbs(t.Path) {
			return fmt.Errorf("target path %q is not absolute", t.Path)
		}
		cleanTarget := filepath.Clean(t.Path)
		if _, ok := seenTargets[cleanTarget]; ok {
			return fmt.Errorf("duplicate target path %q", cleanTarget)
		}
		seenTargets[cleanTarget] = struct{}{}
		if !isValidKind(t.Kind) {
			return fmt.Errorf("invalid target kind %q", t.Kind)
		}
		seenFiles := map[string]struct{}{}
		for _, file := range t.Files {
			if file.Path == "" {
				return fmt.Errorf("target %q has empty file path", t.Path)
			}
			if filepath.IsAbs(file.Path) {
				return fmt.Errorf("target %q file path %q is absolute", t.Path, file.Path)
			}
			if containsTraversalOrDot(file.Path) {
				return fmt.Errorf("target %q file path %q has traversal", t.Path, file.Path)
			}
			cleanFile := filepath.Clean(file.Path)
			if cleanFile == "." {
				return fmt.Errorf("target %q file path %q is invalid", t.Path, file.Path)
			}
			if _, ok := seenFiles[cleanFile]; ok {
				return fmt.Errorf("target %q has duplicate file path %q", t.Path, cleanFile)
			}
			seenFiles[cleanFile] = struct{}{}
			if !isValidSHA256(file.SHA256) {
				return fmt.Errorf("target %q file %q has invalid sha256", t.Path, file.Path)
			}
		}
	}

	seenAffected := map[string]struct{}{}
	for _, affected := range r.AffectedTargets {
		if affected.Path == "" {
			return errors.New("affected path is empty")
		}
		if !filepath.IsAbs(affected.Path) {
			return fmt.Errorf("affected path %q is not absolute", affected.Path)
		}
		cleanAffected := filepath.Clean(affected.Path)
		if _, ok := seenAffected[cleanAffected]; ok {
			return fmt.Errorf("duplicate affected target path %q", cleanAffected)
		}
		seenAffected[cleanAffected] = struct{}{}
		if !isValidAffectedState(affected.State) {
			return fmt.Errorf("invalid affected state %q", affected.State)
		}
		if err := validateRecoveryCommands(affected.Recovery, "affected recovery"); err != nil {
			return err
		}
	}

	return nil
}

func inspectTargets(targets []Target) ([]string, []string, []string, error) {
	verified := make([]string, 0, len(targets))
	missing := []string{}
	drifted := []string{}

	for _, target := range targets {
		cleanTarget := filepath.Clean(target.Path)
		info, err := os.Stat(cleanTarget)
		if err != nil {
			if os.IsNotExist(err) {
				missing = append(missing, cleanTarget)
				continue
			}
			drifted = append(drifted, cleanTarget)
			continue
		}
		if !info.IsDir() && target.Kind != targetKindSymlink {
			drifted = append(drifted, cleanTarget)
			continue
		}
		state := inspectTargetFiles(cleanTarget, target)
		switch state {
		case "missing":
			missing = append(missing, cleanTarget)
		case "drifted":
			drifted = append(drifted, cleanTarget)
		case "verified":
			verified = append(verified, cleanTarget)
		}
	}

	sort.Strings(verified)
	sort.Strings(missing)
	sort.Strings(drifted)
	return verified, missing, drifted, nil
}

func inspectTargetFiles(targetRoot string, target Target) string {
	for _, file := range target.Files {
		filePath := filepath.Join(targetRoot, file.Path)
		expected := strings.ToLower(file.SHA256)
		switch target.Kind {
		case targetKindSymlink:
			got, err := inspectSymlink(filePath)
			if err != nil {
				return fileStatusFromFilesystemErr(err)
			}
			if strings.ToLower(got) != expected {
				return "drifted"
			}
		case targetKindCanonical, targetKindCopy:
			got, err := inspectRegular(filePath)
			if err != nil {
				return fileStatusFromFilesystemErr(err)
			}
			if strings.ToLower(got) != expected {
				return "drifted"
			}
		default:
			return "drifted"
		}
	}
	return "verified"
}

func inspectRegular(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func inspectSymlink(path string) (string, error) {
	dest, err := os.Readlink(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(dest))
	return hex.EncodeToString(sum[:]), nil
}

func fileStatusFromFilesystemErr(err error) string {
	if os.IsNotExist(err) {
		return "missing"
	}
	if strings.Contains(err.Error(), "is a directory") {
		return "drifted"
	}
	return "drifted"
}

func statusFromReceiptOutcomeAndTargets(outcome Outcome, missingTargets, driftedTargets []string) InspectionStatus {
	if len(missingTargets) > 0 {
		return StatusMissing
	}
	if len(driftedTargets) > 0 {
		return StatusDrifted
	}
	switch outcome {
	case OutcomeInstalled:
		return StatusVerified
	case OutcomeBlocked:
		return StatusBlocked
	case OutcomeSkipped:
		return StatusSkipped
	case OutcomeFailed:
		return StatusFailed
	default:
		return StatusFailed
	}
}
func cloneAffectedTargets(targets []AffectedTarget) []AffectedTarget {
	if len(targets) == 0 {
		return nil
	}
	cloned := make([]AffectedTarget, len(targets))
	copy(cloned, targets)
	sort.Slice(cloned, func(i, j int) bool {
		return filepath.Clean(cloned[i].Path) < filepath.Clean(cloned[j].Path)
	})
	return cloned
}

func collectPathSpecificRecovery(targets []AffectedTarget) []string {
	recovery := make([]string, 0)
	for _, target := range targets {
		recovery = append(recovery, target.Recovery...)
	}
	return recovery
}

func shouldExposeGlobalRemove(r Receipt, missingTargets, driftedTargets []string) bool {
	if r.Outcome != OutcomeBlocked {
		return false
	}
	if len(r.Recovery) == 0 || len(r.AffectedTargets) == 0 {
		return false
	}
	if len(missingTargets) > 0 || len(driftedTargets) > 0 {
		return false
	}

	targetPaths := map[string]struct{}{}
	for _, target := range r.Targets {
		targetPaths[filepath.Clean(target.Path)] = struct{}{}
	}

	for _, target := range r.AffectedTargets {
		if len(target.Recovery) == 0 {
			return false
		}
		if target.State != AffectedUnmanaged && target.State != AffectedDrifted {
			return false
		}
		if target.State == AffectedDrifted {
			if _, ok := targetPaths[filepath.Clean(target.Path)]; !ok {
				return false
			}
		}
		if target.State == AffectedUnmanaged {
			if filepath.Base(filepath.Clean(target.Path)) != "pi-worker" {
				return false
			}
			identityPath := filepath.Join(target.Path, IdentityFile)
			identity, err := os.ReadFile(identityPath)
			if err != nil {
				return false
			}
			if string(identity) != IdentityContent {
				return false
			}
		}
	}
	return true
}

func isValidOutcome(outcome Outcome) bool {
	switch outcome {
	case OutcomeInstalled, OutcomeBlocked, OutcomeSkipped, OutcomeFailed:
		return true
	default:
		return false
	}
}

func isValidKind(kind string) bool {
	switch kind {
	case targetKindCanonical, targetKindSymlink, targetKindCopy:
		return true
	default:
		return false
	}
}

func isValidAffectedState(state AffectedState) bool {
	switch state {
	case AffectedUnmanaged, AffectedDrifted, AffectedConflicting:
		return true
	default:
		return false
	}
}

func isValidSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 32 {
		return false
	}
	return true
}

func containsTraversalOrDot(path string) bool {
	if path == "." {
		return true
	}
	parts := strings.Split(filepath.ToSlash(path), "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return true
		}
	}
	return false
}

func validateRecoveryCommands(commands []string, field string) error {
	for i, command := range commands {
		if strings.TrimSpace(command) == "" {
			return fmt.Errorf("%s[%d] must not be empty", field, i)
		}
	}
	return nil
}

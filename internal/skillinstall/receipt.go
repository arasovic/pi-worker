package skillinstall

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	posixpath "path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/arasovic/pi-worker/internal/buildinfo"
)

const SchemaVersion = 1

const IdentityFile = "PI_WORKER_IDENTITY"

const IdentityContent = "pi-worker-skill/v1\n"

const PinnedSkillsVersion = "1.5.23"

const SafeRecoveryCommand = "npm install -g --foreground-scripts pi-worker"

const (
	directoryReadBatchSize = 128
	maxTreeEntries         = 100000
)

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
	StatusStale    InspectionStatus = "stale"
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

type ExternalInspectionState string

const (
	ExternalInspectionPerformed   ExternalInspectionState = "performed"
	ExternalInspectionUnavailable ExternalInspectionState = "unavailable"
)

type ExternalIdentity string

const (
	ExternalIdentityCurrent ExternalIdentity = "current"
	ExternalIdentityLegacy  ExternalIdentity = "legacy"
	ExternalIdentityUnknown ExternalIdentity = "unknown"
	ExternalIdentityNone    ExternalIdentity = "none"
)

type ExternalTarget struct {
	Path     string           `json:"path"`
	Identity ExternalIdentity `json:"identity"`
}

type ExternalInspection struct {
	State   ExternalInspectionState `json:"state"`
	Targets []ExternalTarget        `json:"targets"`
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
	SchemaVersion      int                `json:"schemaVersion"`
	ReceiptPath        string             `json:"receiptPath"`
	Status             InspectionStatus   `json:"status"`
	InstallerVersion   string             `json:"installerVersion,omitempty"`
	ProgramVersion     string             `json:"programVersion,omitempty"`
	VerifiedTargets    []string           `json:"verifiedTargets"`
	TrackedTargets     []string           `json:"trackedTargets"`
	AffectedTargets    []AffectedTarget   `json:"affectedTargets"`
	Recovery           []string           `json:"recovery"`
	ExternalInspection ExternalInspection `json:"externalInspection"`
}

// Load decodes a receipt file and validates it structurally.
func Load(path string) (Receipt, error) {
	data, err := readReceiptBytes(path)
	if err != nil {
		return Receipt{}, fmt.Errorf("load receipt %s: %w", path, err)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
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

	status := statusFromReceiptOutcomeAndTargets(receipt.Outcome, missingTargets, driftedTargets)
	if receipt.Outcome == OutcomeInstalled && !hasVerifiedReceiptEvidence(receipt) {
		verifiedTargets = []string{}
		if status == StatusVerified {
			status = StatusFailed
		}
	}
	current := buildinfo.Current()
	installerVersion := versionWithoutReleasePrefix(receipt.InstallerVersion)
	programVersion := versionWithoutReleasePrefix(current.Version)
	// An intact tree is stale when the receipt names an installer version
	// different from the running program's: the files match the older
	// receipt, not necessarily the skill this program ships. A released
	// binary carries its version as a tag with a leading "v", while the
	// receipt records the bare version, so the comparison drops one leading
	// "v" from either side and the document reports both fields in that same
	// form. A source build names itself "dev", which is not a release
	// identity, so it cannot claim the skill is older than the program and
	// the check does not run.
	if status == StatusVerified && current.Version != "dev" && installerVersion != programVersion {
		status = StatusStale
	}

	insp := Inspection{
		SchemaVersion:    SchemaVersion,
		ReceiptPath:      path,
		Status:           status,
		InstallerVersion: installerVersion,
		ProgramVersion:   programVersion,
		VerifiedTargets:  append([]string{}, verifiedTargets...),
		TrackedTargets:   trackedTargetPaths(receipt.Targets),
		AffectedTargets:  cloneAffectedTargets(receipt.AffectedTargets),
		Recovery:         []string{},
		ExternalInspection: ExternalInspection{
			State:   ExternalInspectionUnavailable,
			Targets: []ExternalTarget{},
		},
	}
	sort.Strings(insp.VerifiedTargets)

	switch {
	case status == StatusStale:
		insp.Recovery = []string{SafeRecoveryCommand}
	case (receipt.Outcome == OutcomeFailed || receipt.Outcome == OutcomeSkipped) && isExactSafeRecovery(receipt.Recovery):
		insp.Recovery = []string{SafeRecoveryCommand}
	case receipt.Outcome != OutcomeFailed && receipt.Outcome != OutcomeSkipped && len(missingTargets) > 0:
		insp.Recovery = []string{SafeRecoveryCommand}
	case shouldExposeGlobalRemove(receipt, missingTargets, driftedTargets):
		insp.Recovery = append([]string{}, receipt.Recovery...)
	}
	return insp, nil
}

// versionWithoutReleasePrefix drops a single leading "v" so a release tag
// ("v0.7.0") and the bare version a receipt records ("0.7.0") are the same
// string.
func versionWithoutReleasePrefix(version string) string {
	if strings.HasPrefix(version, "v") && len(version) > 1 {
		return version[1:]
	}
	return version
}

func trackedTargetPaths(targets []Target) []string {
	seen := map[string]struct{}{}
	paths := []string{}
	for _, target := range targets {
		if target.Kind == targetKindSymlink {
			for _, file := range target.Files {
				candidate := filepath.Clean(filepath.Join(target.Path, filepath.FromSlash(file.Path)))
				if _, ok := seen[candidate]; !ok {
					seen[candidate] = struct{}{}
					paths = append(paths, candidate)
				}
			}
			continue
		}
		candidate := filepath.Clean(target.Path)
		if _, ok := seen[candidate]; !ok {
			seen[candidate] = struct{}{}
			paths = append(paths, candidate)
		}
	}
	sort.Strings(paths)
	return paths
}

func validateReceipt(r Receipt) error {
	stringFields := []struct {
		name  string
		value string
	}{
		{"installerVersion", r.InstallerVersion},
		{"skillsVersion", r.SkillsVersion},
		{"outcome", string(r.Outcome)},
	}
	for _, field := range stringFields {
		if strings.ContainsRune(field.value, '\x00') {
			return fmt.Errorf("%s must not contain NUL", field.name)
		}
	}

	if strings.TrimSpace(r.InstallerVersion) == "" {
		return errors.New("installerVersion must not be empty")
	}
	if strings.TrimSpace(r.SkillsVersion) == "" {
		return errors.New("skillsVersion must not be empty")
	}
	if r.Targets == nil {
		return errors.New("targets must not be null")
	}
	if r.AffectedTargets == nil {
		return errors.New("affectedTargets must not be null")
	}
	if r.Recovery == nil {
		return errors.New("recovery must not be null")
	}
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schemaVersion %d: want %d", r.SchemaVersion, SchemaVersion)
	}
	if !isValidOutcome(r.Outcome) {
		return fmt.Errorf("invalid outcome %q", r.Outcome)
	}

	if r.Outcome == OutcomeInstalled && len(r.Targets) == 0 {
		return errors.New("installed outcome requires at least one target")
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
		if err := rejectNULStrings([]string{t.Path, t.Kind}, fmt.Sprintf("target %q", t.Path)); err != nil {
			return err
		}
		for _, file := range t.Files {
			if err := rejectNULStrings([]string{file.Path, file.SHA256}, fmt.Sprintf("target %q file", t.Path)); err != nil {
				return err
			}
		}
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
		if len(t.Files) == 0 {
			return fmt.Errorf("target %q files must not be empty", t.Path)
		}
		seenFiles := map[string]struct{}{}
		for _, file := range t.Files {
			if file.Path == "" {
				return fmt.Errorf("target %q has empty file path", t.Path)
			}
			if !isValidReceiptFilePath(file.Path) {
				return fmt.Errorf("target %q file path %q is not a normalized POSIX relative path", t.Path, file.Path)
			}
			if _, ok := seenFiles[file.Path]; ok {
				return fmt.Errorf("target %q has duplicate file path %q", t.Path, file.Path)
			}
			seenFiles[file.Path] = struct{}{}
			if !isValidSHA256(file.SHA256) {
				return fmt.Errorf("target %q file %q has invalid sha256", t.Path, file.Path)
			}
		}
	}

	seenAffected := map[string]struct{}{}
	for _, affected := range r.AffectedTargets {
		if err := rejectNULStrings([]string{affected.Path, string(affected.State)}, "affected target"); err != nil {
			return err
		}
		if err := rejectNULStrings(affected.Recovery, "affected recovery"); err != nil {
			return err
		}
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
		if affected.Recovery == nil {
			return errors.New("affected recovery must not be null")
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
		info, err := os.Lstat(cleanTarget)
		if err != nil {
			if os.IsNotExist(err) {
				missing = append(missing, cleanTarget)
				continue
			}
			drifted = append(drifted, cleanTarget)
			continue
		}
		if !info.IsDir() {
			drifted = append(drifted, cleanTarget)
			continue
		}
		state := inspectTargetFiles(cleanTarget, target)
		finalInfo, finalErr := os.Lstat(cleanTarget)
		if finalErr != nil {
			if os.IsNotExist(finalErr) {
				state = "missing"
			} else {
				state = "drifted"
			}
		} else if !finalInfo.IsDir() || !os.SameFile(info, finalInfo) {
			state = "drifted"
		}
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
	switch target.Kind {
	case targetKindSymlink:
		// A symlink target records the directory containing the links. Its
		// receipt intentionally does not describe every entry in that directory.
		for _, file := range target.Files {
			filePath := joinReceiptPath(targetRoot, file.Path)
			got, err := inspectSymlink(filePath)
			if err != nil {
				return fileStatusFromFilesystemErr(err)
			}
			if !strings.EqualFold(got, file.SHA256) {
				return "drifted"
			}
		}
		return "verified"
	case targetKindCanonical, targetKindCopy:
		return inspectExactTree(targetRoot, target.Files)
	default:
		return "drifted"
	}
}

func inspectExactTree(targetRoot string, files []FileHash) string {
	expectedFiles := make(map[string]FileHash, len(files))
	expectedDirs := map[string]struct{}{"": {}}
	for _, file := range files {
		expectedFiles[file.Path] = file
		parts := strings.Split(file.Path, "/")
		for i := 1; i < len(parts); i++ {
			expectedDirs[strings.Join(parts[:i], "/")] = struct{}{}
		}
	}

	seenFiles := make(map[string]struct{}, len(files))
	entryCount := 0
	status := walkExactDirectory(targetRoot, "", expectedFiles, expectedDirs, seenFiles, &entryCount)
	if status != "verified" {
		return status
	}
	if len(seenFiles) != len(expectedFiles) {
		return "missing"
	}
	return "verified"
}

func walkExactDirectory(dirPath, relativeDir string, expectedFiles map[string]FileHash, expectedDirs, seenFiles map[string]struct{}, entryCount *int) string {
	before, err := os.Lstat(dirPath)
	if err != nil {
		return fileStatusFromFilesystemErr(err)
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return "drifted"
	}

	dir, err := openNoFollow(dirPath)
	if err != nil {
		return "drifted"
	}
	defer dir.Close()
	opened, err := dir.Stat()
	if err != nil || !opened.IsDir() || opened.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, opened) {
		return "drifted"
	}

	for {
		entries, readErr := dir.Readdir(directoryReadBatchSize)
		for _, entry := range entries {
			(*entryCount)++
			if *entryCount > maxTreeEntries {
				return "drifted"
			}

			name := entry.Name()
			relativePath := name
			if relativeDir != "" {
				relativePath = relativeDir + "/" + name
			}
			childPath := filepath.Join(dirPath, name)
			child, lstatErr := os.Lstat(childPath)
			if lstatErr != nil {
				return fileStatusFromFilesystemErr(lstatErr)
			}
			if child.Mode()&os.ModeSymlink != 0 {
				return "drifted"
			}
			if child.IsDir() {
				if _, ok := expectedDirs[relativePath]; !ok {
					return "drifted"
				}
				if status := walkExactDirectory(childPath, relativePath, expectedFiles, expectedDirs, seenFiles, entryCount); status != "verified" {
					return status
				}
				continue
			}
			if !child.Mode().IsRegular() {
				return "drifted"
			}
			expected, ok := expectedFiles[relativePath]
			if !ok {
				return "drifted"
			}
			got, hashErr := inspectRegular(childPath)
			if hashErr != nil {
				return "drifted"
			}
			if !strings.EqualFold(got, expected.SHA256) {
				return "drifted"
			}
			seenFiles[relativePath] = struct{}{}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "drifted"
		}
	}

	after, err := os.Lstat(dirPath)
	if err != nil {
		return fileStatusFromFilesystemErr(err)
	}
	if !after.IsDir() || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, after) {
		return "drifted"
	}
	return "verified"
}

func inspectRegular(path string) (sum string, err error) {
	before, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !before.Mode().IsRegular() {
		return "", errors.New("managed path is not a regular file")
	}

	f, err := openNoFollow(path)
	if err != nil {
		return "", err
	}
	defer func() {
		if closeErr := f.Close(); err == nil && closeErr != nil {
			sum = ""
			err = closeErr
		}
	}()

	opened, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(before, opened) {
		return "", errors.New("managed file changed before hashing")
	}

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return "", err
	}

	after, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return "", errors.New("managed file changed after hashing")
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
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
	cloned := make([]AffectedTarget, len(targets))
	copy(cloned, targets)
	sort.Slice(cloned, func(i, j int) bool {
		return filepath.Clean(cloned[i].Path) < filepath.Clean(cloned[j].Path)
	})
	return cloned
}

func shouldExposeGlobalRemove(r Receipt, missingTargets, driftedTargets []string) bool {
	if r.Outcome != OutcomeBlocked || len(missingTargets) > 0 || !isExactGlobalRecovery(r.Recovery) {
		return false
	}

	targetPaths := map[string]struct{}{}
	for _, target := range r.Targets {
		targetPaths[filepath.Clean(target.Path)] = struct{}{}
	}

	affectedDrifted := map[string]struct{}{}
	for _, affected := range r.AffectedTargets {
		switch affected.State {
		case AffectedUnmanaged:
			if filepath.Base(filepath.Clean(affected.Path)) != "pi-worker" || !hasValidSkillIdentity(affected.Path) || !hasExactPathRecovery(affected) {
				return false
			}
		case AffectedDrifted:
			cleanPath := filepath.Clean(affected.Path)
			if _, ok := targetPaths[cleanPath]; !ok {
				return false
			}
			if !hasExactPathRecovery(affected) {
				return false
			}
			affectedDrifted[cleanPath] = struct{}{}
		default:
			return false
		}
	}
	if len(affectedDrifted) != len(driftedTargets) {
		return false
	}
	for _, drifted := range driftedTargets {
		if _, ok := affectedDrifted[filepath.Clean(drifted)]; !ok {
			return false
		}
	}
	return true
}

func isExactGlobalRecovery(commands []string) bool {
	return len(commands) == 2 &&
		commands[0] == "npx --yes skills@"+PinnedSkillsVersion+" remove pi-worker -g -y" &&
		commands[1] == SafeRecoveryCommand
}

func isExactSafeRecovery(commands []string) bool {
	return len(commands) == 1 && commands[0] == SafeRecoveryCommand
}

func hasExactPathRecovery(affected AffectedTarget) bool {
	return len(affected.Recovery) > 0 && affected.Recovery[0] == "Inspect and back up "+affected.Path+" before retrying."
}

func hasValidSkillIdentity(targetPath string) bool {
	root, err := os.Lstat(targetPath)
	if err != nil || !root.IsDir() || root.Mode()&os.ModeSymlink != 0 {
		return false
	}

	identity, err := readBoundedRegularFile(filepath.Join(targetPath, IdentityFile), int64(len(IdentityContent)))
	if err != nil || string(identity) != IdentityContent {
		return false
	}

	skill, err := readBoundedRegularFile(filepath.Join(targetPath, "SKILL.md"), maxReceiptBytes)
	if err != nil {
		return false
	}
	if !hasPiWorkerFrontMatter(skill) {
		return false
	}
	after, err := os.Lstat(targetPath)
	return err == nil && after.IsDir() && after.Mode()&os.ModeSymlink == 0 && os.SameFile(root, after)
}

func hasPiWorkerFrontMatter(data []byte) bool {
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || strings.TrimSuffix(lines[0], "\r") != "---" {
		return false
	}

	nameCount := 0
	for _, rawLine := range lines[1:] {
		line := strings.TrimSuffix(rawLine, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			return nameCount == 1
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		separator := strings.IndexByte(line, ':')
		if separator < 0 {
			return false
		}
		key := strings.TrimSpace(line[:separator])
		value := strings.TrimSpace(line[separator+1:])
		if !isFrontMatterKey(key) || value == "" {
			return false
		}
		if key == "name" {
			nameCount++
			if nameCount != 1 || !frontMatterValueIsPiWorker(value) {
				return false
			}
		}
	}
	return false
}

func isFrontMatterKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func frontMatterValueIsPiWorker(value string) bool {
	if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') ||
		(value[0] == '\'' && value[len(value)-1] == '\'')) {
		value = value[1 : len(value)-1]
	}
	return value == "pi-worker"
}

func hasVerifiedReceiptEvidence(r Receipt) bool {
	if r.Outcome != OutcomeInstalled || r.SkillsVersion != PinnedSkillsVersion {
		return false
	}
	canonicalTargets := []Target{}
	for _, target := range r.Targets {
		if target.Kind == targetKindCanonical {
			canonicalTargets = append(canonicalTargets, target)
		}
	}
	if len(canonicalTargets) != 1 {
		return false
	}
	target := canonicalTargets[0]
	if !hasValidSkillIdentity(target.Path) {
		return false
	}

	identityHash := sha256.Sum256([]byte(IdentityContent))
	wantIdentityHash := hex.EncodeToString(identityHash[:])
	recordedIdentity := false
	recordedSkill := false
	for _, file := range target.Files {
		switch filepath.Clean(file.Path) {
		case IdentityFile:
			if !strings.EqualFold(file.SHA256, wantIdentityHash) {
				return false
			}
			recordedIdentity = true
		case "SKILL.md":
			recordedSkill = true
		}
	}
	return recordedIdentity && recordedSkill
}

func rejectNULStrings(values []string, field string) error {
	for _, value := range values {
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("%s must not contain NUL", field)
		}
	}
	return nil
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

func joinReceiptPath(root, filePath string) string {
	parts := append([]string{root}, strings.Split(filePath, "/")...)
	return filepath.Join(parts...)
}

func isValidReceiptFilePath(filePath string) bool {
	if filePath == "" || strings.ContainsRune(filePath, '\\') || posixpath.IsAbs(filePath) {
		return false
	}
	if filePath == "." || posixpath.Clean(filePath) != filePath {
		return false
	}
	for _, part := range strings.Split(filePath, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func validateRecoveryCommands(commands []string, field string) error {
	for i, command := range commands {
		if strings.ContainsRune(command, '\x00') {
			return fmt.Errorf("%s[%d] must not contain NUL", field, i)
		}
		if strings.TrimSpace(command) == "" {
			return fmt.Errorf("%s[%d] must not be empty", field, i)
		}
	}
	return nil
}

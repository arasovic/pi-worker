package releaseartifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Target identifies one release binary target.
type Target struct {
	GOOS   string
	GOARCH string
}

// Targets is the fixed native build matrix.
var Targets = []Target{
	{GOOS: "darwin", GOARCH: "arm64"},
	{GOOS: "darwin", GOARCH: "amd64"},
	{GOOS: "linux", GOARCH: "arm64"},
	{GOOS: "linux", GOARCH: "amd64"},
}

// Options configure one release build invocation.
type Options struct {
	Version   string
	Commit    string
	BuildDate time.Time
	OutputDir string
}

type commandRunner func(ctx context.Context, cmd *exec.Cmd) error

var runCommand = func(ctx context.Context, cmd *exec.Cmd) error {
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, bytes.TrimSpace(output))
	}
	return nil
}

const (
	cmdBuild       = "go"
	cmdGoBuild     = "build"
	cmdTrimPath    = "-trimpath"
	cmdBuildVCS    = "-buildvcs=false"
	cmdOutputFlag  = "-o"
	cmdLDFL        = "-ldflags"
	cmdPackagePath = "./cmd/pi-worker"

	licenseFile = "LICENSE"
	noticeFile  = "THIRD_PARTY_NOTICES"
)

var supportedTargets = map[string]struct{}{
	"darwin/arm64": {},
	"darwin/amd64": {},
	"linux/arm64":  {},
	"linux/amd64":  {},
}

// Build cross-builds release artifacts for all Targets into OutputDir.
func Build(ctx context.Context, options Options) (err error) {
	if err := validateOptions(options); err != nil {
		return err
	}
	if err := validateTargets(Targets); err != nil {
		return err
	}

	root, err := RepositoryRoot()
	if err != nil {
		return err
	}

	stagingDir, err := os.MkdirTemp(filepath.Dir(options.OutputDir), "."+filepath.Base(options.OutputDir)+".staging-")
	if err != nil {
		return fmt.Errorf("create staging directory: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if cleanupErr := os.RemoveAll(stagingDir); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove staging directory: %w", cleanupErr))
		}
	}()

	licensePath := filepath.Join(root, licenseFile)
	noticePath := filepath.Join(root, noticeFile)
	archives := make([]string, 0, len(Targets))

	targets := append([]Target(nil), Targets...)
	for _, target := range targets {
		targetArchive := filepath.Join(stagingDir, archiveName(options.Version, target))
		tempDir, err := os.MkdirTemp("", "pi-worker-release-")
		if err != nil {
			return fmt.Errorf("create temporary build directory: %w", err)
		}
		if err := buildOneTarget(ctx, target, options, root, tempDir, targetArchive, licensePath, noticePath); err != nil {
			_ = os.RemoveAll(tempDir)
			return err
		}
		if err := os.RemoveAll(tempDir); err != nil {
			return fmt.Errorf("remove temporary build directory: %w", err)
		}

		archives = append(archives, filepath.Base(targetArchive))
	}

	if err := writeChecksums(stagingDir, archives); err != nil {
		return err
	}
	if err := os.Chmod(stagingDir, 0o755); err != nil {
		return fmt.Errorf("set output directory permissions: %w", err)
	}
	if err := os.Rename(stagingDir, options.OutputDir); err != nil {
		return fmt.Errorf("publish output directory: %w", err)
	}
	committed = true
	return nil
}

func buildOneTarget(ctx context.Context, target Target, options Options, root, tempDir, outputPath, licensePath, noticePath string) error {
	binaryPath := filepath.Join(tempDir, "pi-worker")
	buildEnv := targetEnvironment(target)
	if err := runGoBuild(ctx, root, target, options, binaryPath, buildEnv); err != nil {
		return err
	}
	if err := createArchive(binaryPath, licensePath, noticePath, outputPath, options.BuildDate); err != nil {
		return err
	}
	return nil
}

func runGoBuild(ctx context.Context, root string, target Target, options Options, binaryPath string, buildEnv []string) error {
	ldflags := []string{
		"-s",
		"-w",
		"-X", "github.com/arasovic/pi-worker/internal/buildinfo.Version=" + options.Version,
		"-X", "github.com/arasovic/pi-worker/internal/buildinfo.Commit=" + options.Commit,
		"-X", "github.com/arasovic/pi-worker/internal/buildinfo.BuildDate=" + options.BuildDate.Format(time.RFC3339),
	}
	args := []string{
		cmdGoBuild,
		cmdTrimPath,
		cmdBuildVCS,
		cmdOutputFlag,
		binaryPath,
		cmdLDFL,
		strings.Join(ldflags, " "),
		cmdPackagePath,
	}

	cmd := exec.CommandContext(ctx, cmdBuild, args...)
	cmd.Dir = root
	cmd.Env = buildEnv
	if err := runCommand(ctx, cmd); err != nil {
		return fmt.Errorf("build %s/%s: %w", target.GOOS, target.GOARCH, err)
	}
	return nil
}

func writeChecksums(outputDir string, archives []string) error {
	sort.Strings(archives)
	lines := make([]byte, 0, len(archives)*80)
	for _, archiveName := range archives {
		path := filepath.Join(outputDir, archiveName)
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read archive %s for checksum: %w", archiveName, err)
		}
		sum := sha256.Sum256(data)
		line := fmt.Sprintf("%x  %s\n", sum, archiveName)
		lines = append(lines, []byte(line)...)
	}
	if err := os.WriteFile(filepath.Join(outputDir, "checksums.txt"), lines, 0o644); err != nil {
		return fmt.Errorf("write checksums: %w", err)
	}
	return nil
}

// RepositoryRoot discovers the repository containing the current working
// directory.
func RepositoryRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("determine working directory: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(cwd, "go.mod")); err == nil {
			if _, err := os.Stat(filepath.Join(cwd, licenseFile)); err == nil {
				if _, err := os.Stat(filepath.Join(cwd, cmdPackagePath)); err == nil {
					return cwd, nil
				}
			}
		}
		parent := filepath.Dir(cwd)
		if parent == cwd {
			return "", fmt.Errorf("repository root not found")
		}
		cwd = parent
	}
}

func archiveName(version string, target Target) string {
	return fmt.Sprintf("pi-worker_%s_%s_%s.tar.gz", version, target.GOOS, target.GOARCH)
}

func targetEnvironment(target Target) []string {
	return targetEnvironmentFrom(os.Environ(), target)
}

func targetEnvironmentFrom(hostEnv []string, target Target) []string {
	env := append([]string(nil), hostEnv...)
	for _, setting := range []struct {
		key   string
		value string
	}{
		{key: "GOPROXY", value: "off"},
		{key: "GOSUMDB", value: "off"},
		{key: "GOTOOLCHAIN", value: "local"},
		{key: "GOWORK", value: "off"},
		{key: "GOFLAGS", value: "-mod=readonly"},
		{key: "GOENV", value: "off"},
		{key: "GOTELEMETRY", value: "off"},
		{key: "GOEXPERIMENT", value: "none"},
		{key: "GOFIPS140", value: "off"},
		{key: "GOAMD64", value: "v1"},
		{key: "GOARM64", value: "v8.0"},
		{key: "GOOS", value: target.GOOS},
		{key: "GOARCH", value: target.GOARCH},
		{key: "CGO_ENABLED", value: "0"},
	} {
		env = setEnv(env, setting.key, setting.value)
	}
	return env
}

func setEnv(env []string, key, value string) []string {
	updated := false
	filtered := make([]string, 0, len(env)+1)
	prefix := key + "="
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			filtered = append(filtered, entry)
			continue
		}
		if !updated {
			filtered = append(filtered, key+"="+value)
			updated = true
		}
	}
	if !updated {
		return append(filtered, key+"="+value)
	}
	return filtered
}

func validateOptions(options Options) error {
	if !validSemVer(options.Version) {
		return fmt.Errorf("invalid version %q", options.Version)
	}
	if !commitPattern.MatchString(options.Commit) {
		return fmt.Errorf("invalid commit %q", options.Commit)
	}
	if options.BuildDate.Location() != time.UTC {
		return fmt.Errorf("build date must be UTC")
	}
	if options.OutputDir == "" {
		return fmt.Errorf("output directory is required")
	}
	info, err := os.Lstat(options.OutputDir)
	if err == nil {
		if info.IsDir() {
			return fmt.Errorf("output directory already exists")
		}
		return fmt.Errorf("output path exists and is not a directory")
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("output directory check failed: %w", err)
	}
	return nil
}

func validSemVer(version string) bool {
	if len(version) < 2 || version[0] != 'v' {
		return false
	}

	value := version[1:]
	coreAndPrerelease := value
	build := ""
	if plus := strings.IndexByte(value, '+'); plus >= 0 {
		coreAndPrerelease = value[:plus]
		build = value[plus+1:]
		if strings.ContainsRune(build, '+') || !validIdentifiers(build, false) {
			return false
		}
	}

	core := coreAndPrerelease
	prerelease := ""
	if dash := strings.IndexByte(coreAndPrerelease, '-'); dash >= 0 {
		core = coreAndPrerelease[:dash]
		prerelease = coreAndPrerelease[dash+1:]
		if !validIdentifiers(prerelease, true) {
			return false
		}
	}

	coreParts := strings.Split(core, ".")
	if len(coreParts) != 3 {
		return false
	}
	for _, part := range coreParts {
		if !validNumericIdentifier(part) {
			return false
		}
	}
	return true
}

func validIdentifiers(value string, prerelease bool) bool {
	if value == "" {
		return false
	}
	for _, identifier := range strings.Split(value, ".") {
		if identifier == "" || !validIdentifierCharacters(identifier) {
			return false
		}
		if prerelease && allASCIIDigits(identifier) && len(identifier) > 1 && identifier[0] == '0' {
			return false
		}
	}
	return true
}

func validNumericIdentifier(value string) bool {
	return value != "" && allASCIIDigits(value) && (len(value) == 1 || value[0] != '0')
}

func allASCIIDigits(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func validIdentifierCharacters(value string) bool {
	for i := 0; i < len(value); i++ {
		char := value[i]
		if (char >= '0' && char <= '9') ||
			(char >= 'A' && char <= 'Z') ||
			(char >= 'a' && char <= 'z') ||
			char == '-' {
			continue
		}
		return false
	}
	return true
}

func validateTargets(targets []Target) error {
	if len(targets) == 0 {
		return fmt.Errorf("no targets configured")
	}
	if len(targets) != len(supportedTargets) {
		return fmt.Errorf("exactly four targets are required")
	}
	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if target.GOOS == "" || target.GOARCH == "" {
			return fmt.Errorf("target missing GOOS or GOARCH")
		}
		key := target.GOOS + "/" + target.GOARCH
		if _, ok := supportedTargets[key]; !ok {
			return fmt.Errorf("unsupported target %s", key)
		}
		if _, ok := seen[key]; ok {
			return fmt.Errorf("duplicate target %s", key)
		}
		seen[key] = struct{}{}
	}
	return nil
}

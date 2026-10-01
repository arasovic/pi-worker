package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	configpkg "github.com/arasovic/pi-worker/internal/config"
	"github.com/arasovic/pi-worker/internal/pi"
)

const defaultConfigTimeout = 30 * time.Second

var userConfigPath = configpkg.UserPath

// configuredRunConfigError marks a failure while loading run configuration
// (the default model and machine concurrency limit) from the persisted
// configuration file. It is distinct from the ordinary input-resolution
// errors: a missing or empty default is a usage error, but an invalid
// on-disk config is an internal failure, just like config show reports it.
type configuredRunConfigError struct {
	err error
}

func (e *configuredRunConfigError) Error() string { return e.err.Error() }

func (e *configuredRunConfigError) Unwrap() error { return e.err }

type configOptions struct {
	command    string
	setter     string // "default-model" or "max-model-workers"
	json       bool
	model      string
	maxWorkers int
	timeout    time.Duration
	debug      bool
}

func configCommand(parent context.Context, args []string, stdout, stderr io.Writer) int {
	opts, err := parseConfigArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: %v\n", err)
		printUsage(stderr)
		return 2
	}
	path, err := userConfigPath()
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: determine config path: %v\n", err)
		return 9
	}
	if opts.command == "show" {
		cfg, err := configpkg.Load(path)
		if errors.Is(err, fs.ErrNotExist) {
			cfg = configpkg.Empty()
		} else if err != nil {
			fmt.Fprintf(stderr, "pi-worker: load config: %v\n", err)
			return 9
		}
		if opts.json {
			data, err := json.Marshal(cfg)
			if err != nil {
				fmt.Fprintf(stderr, "pi-worker: encode config: %v\n", err)
				return 9
			}
			fmt.Fprintln(stdout, string(data))
			return 0
		}
		fmt.Fprintf(stdout, "default-model: %s\n", cfg.DefaultModel)
		fmt.Fprintf(stdout, "max-model-workers: %d\n", cfg.MaxModelWorkers)
		return 0
	}

	// Load the document once up front so a broken file fails before any work
	// (for default-model, before the catalog is queried). The value is not
	// kept: saveConfigField reloads it immediately before writing.
	if _, err := loadConfig(path); err != nil {
		fmt.Fprintf(stderr, "pi-worker: load config: %v\n", err)
		return 9
	}

	if opts.setter == "max-model-workers" {
		if err := saveConfigField(path, func(cfg *configpkg.Config) {
			cfg.MaxModelWorkers = opts.maxWorkers
		}); err != nil {
			fmt.Fprintf(stderr, "pi-worker: save config: %v\n", err)
			return 9
		}
		fmt.Fprintf(stdout, "max-model-workers: %d\n", opts.maxWorkers)
		return 0
	}

	workspace, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: determine workspace: %v\n", err)
		return 9
	}
	ctx, cancel := context.WithTimeout(parent, opts.timeout)
	defer cancel()
	var debug *pi.DebugSink
	if opts.debug {
		debug = pi.NewDebugSink(stderr)
	}
	models, err := newCatalog().List(ctx, pi.CatalogRequest{Workspace: workspace, Debug: debug})
	if err != nil {
		return modelsErrorCode(ctx, err, stderr)
	}
	if err := ctx.Err(); err != nil {
		return modelsErrorCode(ctx, err, stderr)
	}
	if !catalogContains(models, opts.model) {
		fmt.Fprintf(stderr, "pi-worker: model %q is not in the available catalog; no fallback attempted\n", opts.model)
		return 3
	}
	if err := ctx.Err(); err != nil {
		return modelsErrorCode(ctx, err, stderr)
	}
	if err := saveConfigField(path, func(cfg *configpkg.Config) {
		cfg.DefaultModel = opts.model
	}); err != nil {
		fmt.Fprintf(stderr, "pi-worker: save config: %v\n", err)
		return 9
	}
	fmt.Fprintf(stdout, "default-model: %s\n", opts.model)
	return 0
}

// saveConfigField persists one changed configuration field. The command
// loads the document once up front so a broken file fails before any work,
// but that first copy is stale by the time a slow catalog query finishes;
// saving it back would silently revert a value another `config set` wrote in
// the meantime. This helper reloads the document immediately before the
// save and sets only the field the caller changes, so every other on-disk
// field is carried forward. A missing file is the empty configuration, as on
// the initial load.
//
// ponytail: two setters within the same few milliseconds can still lose one
// update, because the reload and the save are not atomic together. The
// upgrade is a file lock around the whole load-and-save.
func saveConfigField(path string, apply func(*configpkg.Config)) error {
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	apply(&cfg)
	return configpkg.Save(path, cfg)
}

// loadConfig loads the configuration document at path, treating a missing
// file as the empty configuration exactly as the initial load does.
func loadConfig(path string) (configpkg.Config, error) {
	cfg, err := configpkg.Load(path)
	if errors.Is(err, fs.ErrNotExist) {
		return configpkg.Empty(), nil
	}
	return cfg, err
}

func parseConfigArgs(args []string) (configOptions, error) {
	opts := configOptions{timeout: defaultConfigTimeout}
	if len(args) == 0 {
		return opts, errors.New("config requires a subcommand")
	}
	switch args[0] {
	case "show":
		opts.command = "show"
		if len(args) == 1 {
			return opts, nil
		}
		if len(args) == 2 && args[1] == "--json" {
			opts.json = true
			return opts, nil
		}
		return opts, fmt.Errorf("invalid config show syntax")
	case "set":
		opts.command = "set"
		if len(args) < 3 {
			return opts, fmt.Errorf("invalid config set syntax")
		}
		switch args[1] {
		case "default-model":
			opts.setter = "default-model"
			opts.model = args[2]
			if err := validateModel(opts.model); err != nil {
				return opts, err
			}
			seen := make(map[string]bool)
			for i := 3; i < len(args); i++ {
				name, value, hasValue := strings.Cut(args[i], "=")
				switch name {
				case "--debug":
					if hasValue || seen[name] {
						return opts, fmt.Errorf("invalid flag %s", name)
					}
					seen[name] = true
					opts.debug = true
				case "--timeout":
					if !hasValue {
						if i+1 >= len(args) {
							return opts, fmt.Errorf("flag %s requires a value", name)
						}
						i++
						value = args[i]
					}
					if seen[name] {
						return opts, fmt.Errorf("flag %s specified more than once", name)
					}
					seen[name] = true
					duration, err := time.ParseDuration(value)
					if err != nil || duration <= 0 {
						return opts, fmt.Errorf("invalid timeout %q", value)
					}
					opts.timeout = duration
				default:
					return opts, fmt.Errorf("unknown flag %q", args[i])
				}
			}
		case "max-model-workers":
			opts.setter = "max-model-workers"
			n, err := strconv.Atoi(args[2])
			if err != nil || n <= 0 {
				return opts, fmt.Errorf("invalid max-model-workers %q: must be a positive integer", args[2])
			}
			opts.maxWorkers = n
			if len(args) > 3 {
				return opts, fmt.Errorf("invalid config set syntax")
			}
		default:
			return opts, fmt.Errorf("invalid config set syntax")
		}
		return opts, nil
	default:
		return opts, fmt.Errorf("unknown config subcommand %q", args[0])
	}
}

func catalogContains(models []pi.ModelProjection, selector string) bool {
	provider, id, ok := strings.Cut(selector, "/")
	if !ok || provider == "" || id == "" {
		return false
	}
	for _, model := range models {
		if model.Provider == provider && model.ID == id {
			return true
		}
	}
	return false
}

// runSettings holds the foreground-run settings resolved from the
// persisted configuration file exactly once per syntactically valid
// run, whether or not every task carries an explicit --model.
type runSettings struct {
	defaultModel    string
	maxModelWorkers int
	admissionRoot   string
}

// configuredRunSettings resolves the configuration file once and returns
// the full set of run settings. A missing file yields configpkg.Empty();
// path-resolution, load, or validation failures are internal run-input
// errors mapped to exit 9 by reportRunInputError.
func configuredRunSettings() (runSettings, error) {
	path, err := userConfigPath()
	if err != nil {
		return runSettings{}, &configuredRunConfigError{err: fmt.Errorf("determine config path: %w", err)}
	}
	cfg, err := configpkg.Load(path)
	if errors.Is(err, fs.ErrNotExist) {
		cfg = configpkg.Empty()
	} else if err != nil {
		return runSettings{}, &configuredRunConfigError{err: fmt.Errorf("load config: %w", err)}
	}
	admissionRoot := filepath.Join(filepath.Dir(path), "admission")
	return runSettings{
		defaultModel:    cfg.DefaultModel,
		maxModelWorkers: cfg.MaxModelWorkers,
		admissionRoot:   admissionRoot,
	}, nil
}

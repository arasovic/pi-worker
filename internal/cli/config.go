package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	configpkg "github.com/arasovic/pi-worker/internal/config"
	"github.com/arasovic/pi-worker/internal/pi"
)

const defaultConfigTimeout = 30 * time.Second

var userConfigPath = configpkg.UserPath

type configOptions struct {
	command string
	json    bool
	model   string
	timeout time.Duration
	debug   bool
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
		if err != nil {
			fmt.Fprintf(stderr, "pi-worker: load config: %v\n", err)
			return 2
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
	if err := configpkg.Save(path, configpkg.Config{SchemaVersion: 1, DefaultModel: opts.model}); err != nil {
		fmt.Fprintf(stderr, "pi-worker: save config: %v\n", err)
		return 9
	}
	fmt.Fprintf(stdout, "default-model: %s\n", opts.model)
	return 0
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
		if len(args) < 3 || args[1] != "default-model" {
			return opts, fmt.Errorf("invalid config set syntax")
		}
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
		return opts, nil
	default:
		return opts, fmt.Errorf("unknown config subcommand %q", args[0])
	}
}

func catalogContains(models []pi.ModelProjection, selector string) bool {
	provider, id, _ := strings.Cut(selector, "/")
	for _, model := range models {
		if model.Provider == provider && model.ID == id {
			return true
		}
	}
	return false
}

func configuredRunModel() (string, error) {
	path, err := userConfigPath()
	if err != nil {
		return "", fmt.Errorf("determine config path: %w", err)
	}
	cfg, err := configpkg.Load(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", errors.New("missing required flag --model and no configured default model")
	}
	if err != nil {
		return "", fmt.Errorf("load configured default: %w", err)
	}
	if cfg.DefaultModel == "" {
		return "", errors.New("missing required flag --model and no configured default model")
	}
	return cfg.DefaultModel, nil
}

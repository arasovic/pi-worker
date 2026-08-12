package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/arasovic/pi-worker/internal/pi"
)

const defaultModelsTimeout = 30 * time.Second

type modelsOptions struct {
	timeout time.Duration
	json    bool
	debug   bool
}

type modelsOutput struct {
	SchemaVersion int           `json:"schemaVersion"`
	Models        []modelOutput `json:"models"`
}

type modelOutput struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Selector string `json:"selector"`
}

func modelsCommand(parent context.Context, args []string, stdout, stderr io.Writer) int {
	opts, err := parseModelsArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: %v\n", err)
		printUsage(stderr)
		return 2
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
	slices.SortFunc(models, func(a, b pi.ModelProjection) int {
		if a.Provider != b.Provider {
			return strings.Compare(a.Provider, b.Provider)
		}
		return strings.Compare(a.ID, b.ID)
	})
	if opts.json {
		output := modelsOutput{SchemaVersion: 1, Models: make([]modelOutput, len(models))}
		for i, model := range models {
			selector, ok := pi.ExactModelSelector(model.Provider, model.ID)
			if !ok {
				return modelsErrorCode(ctx, &pi.ProtocolError{Message: "catalog entry has invalid provider or id"}, stderr)
			}
			output.Models[i] = modelOutput{Provider: model.Provider, ID: model.ID, Selector: selector}
		}
		data, err := json.Marshal(output)
		if err != nil {
			fmt.Fprintf(stderr, "pi-worker: encode models: %v\n", err)
			return 9
		}
		fmt.Fprintln(stdout, string(data))
		return 0
	}
	for _, model := range models {
		selector, ok := pi.ExactModelSelector(model.Provider, model.ID)
		if !ok {
			return modelsErrorCode(ctx, &pi.ProtocolError{Message: "catalog entry has invalid provider or id"}, stderr)
		}
		fmt.Fprintln(stdout, selector)
	}
	return 0
}

func parseModelsArgs(args []string) (modelsOptions, error) {
	opts := modelsOptions{timeout: defaultModelsTimeout}
	seen := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
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
			if err != nil {
				return opts, fmt.Errorf("invalid timeout %q: %v", value, err)
			}
			if duration <= 0 {
				return opts, fmt.Errorf("invalid timeout %q: must be positive", value)
			}
			opts.timeout = duration
		case "--json", "--debug":
			if hasValue {
				return opts, fmt.Errorf("flag %s does not take a value", name)
			}
			if seen[name] {
				return opts, fmt.Errorf("flag %s specified more than once", name)
			}
			seen[name] = true
			if name == "--json" {
				opts.json = true
			} else {
				opts.debug = true
			}
		default:
			if strings.HasPrefix(arg, "-") {
				return opts, fmt.Errorf("unknown flag %q", arg)
			}
			return opts, fmt.Errorf("unexpected argument %q", arg)
		}
	}
	return opts, nil
}

func modelsErrorCode(ctx context.Context, err error, stderr io.Writer) int {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		fmt.Fprintf(stderr, "pi-worker: timed out: %v\n", err)
		return 7
	case errors.Is(ctx.Err(), context.Canceled):
		fmt.Fprintf(stderr, "pi-worker: cancelled: %v\n", err)
		return 8
	}
	var readiness *pi.ReadinessError
	if errors.As(err, &readiness) {
		fmt.Fprintf(stderr, "pi-worker: %v\n", err)
		return 3
	}
	var protocol *pi.ProtocolError
	if errors.As(err, &protocol) {
		fmt.Fprintf(stderr, "pi-worker: %v\n", err)
		return 9
	}
	fmt.Fprintf(stderr, "pi-worker: %v\n", err)
	return 9
}

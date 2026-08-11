package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"pi-worker/internal/doctor"
	"pi-worker/internal/pi"
)

const defaultDoctorTimeout = 30 * time.Second

var newDoctorDependencies = doctor.DefaultDependencies

type doctorOptions struct {
	timeout time.Duration
	json    bool
	debug   bool
}

func doctorCommand(parent context.Context, args []string, stdout, stderr io.Writer) int {
	opts, err := parseDoctorArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: %v\n", err)
		printUsage(stderr)
		return 2
	}
	ctx, cancel := context.WithTimeout(parent, opts.timeout)
	defer cancel()

	var debug *pi.DebugSink
	if opts.debug {
		debug = pi.NewDebugSink(stderr)
	}
	deps := newDoctorDependencies(debug)
	result, runErr := doctor.Run(ctx, deps)
	code := doctorExitCode(ctx, result, runErr, stderr)
	if runErr != nil {
		// An aborted inspection has no complete result to report. Rendering a
		// partial result could claim readiness that the remaining checks never
		// established.
		return code
	}

	if opts.json {
		data, err := json.Marshal(result)
		if err != nil {
			fmt.Fprintf(stderr, "pi-worker: encode doctor result: %v\n", err)
			return 9
		}
		fmt.Fprintln(stdout, string(data))
		return code
	}
	for _, check := range result.Checks {
		fmt.Fprintf(stdout, "%s: %s - %s\n", check.Name, check.Status, check.Message)
	}
	if result.Ready {
		fmt.Fprintln(stdout, "ready: yes")
	} else {
		fmt.Fprintln(stdout, "ready: no")
	}
	return code
}

func parseDoctorArgs(args []string) (doctorOptions, error) {
	opts := doctorOptions{timeout: defaultDoctorTimeout}
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

func doctorExitCode(ctx context.Context, result doctor.Result, err error, stderr io.Writer) int {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		fmt.Fprintln(stderr, "pi-worker: doctor timed out")
		return 7
	case errors.Is(ctx.Err(), context.Canceled):
		fmt.Fprintln(stderr, "pi-worker: doctor cancelled")
		return 8
	case err != nil && doctor.FailureKindOf(err) == doctor.FailureTimeout:
		fmt.Fprintln(stderr, "pi-worker: doctor timed out")
		return 7
	case err != nil && doctor.FailureKindOf(err) == doctor.FailureCancellation:
		fmt.Fprintln(stderr, "pi-worker: doctor cancelled")
		return 8
	case err != nil:
		fmt.Fprintln(stderr, "pi-worker: doctor encountered an internal error")
		return 9
	case !result.Ready:
		return 3
	default:
		return 0
	}
}

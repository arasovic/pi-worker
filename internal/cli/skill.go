package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"pi-worker/internal/skillinstall"
)

var resolveSkillReceiptPath = skillinstall.UserReceiptPath
var inspectSkillReceipt = skillinstall.Inspect

type skillOptions struct {
	command string
	json    bool
}

// skillCommand executes read-only skill installation reporting commands.
func skillCommand(parent context.Context, args []string, stdout, stderr io.Writer) int {
	opts, err := parseSkillArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: %v\n", err)
		printUsage(stderr)
		return 2
	}
	if code, handled := skillContextExit(parent, stderr); handled {
		return code
	}

	switch opts.command {
	case "receipt-path":
		return skillReceiptPathCommand(parent, opts, stdout, stderr)
	case "status":
		return skillStatusCommand(parent, opts, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "pi-worker: unknown skill command %q\n", opts.command)
		printUsage(stderr)
		return 2
	}
}

func skillReceiptPathCommand(parent context.Context, opts skillOptions, stdout, stderr io.Writer) int {
	path, err := resolveSkillReceiptPath()
	if code, handled := skillContextExit(parent, stderr); handled {
		return code
	}
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: determine skill receipt path: %v\n", err)
		return 9
	}
	if !filepath.IsAbs(path) {
		fmt.Fprintf(stderr, "pi-worker: skill receipt path must be absolute: %q\n", path)
		return 9
	}

	if opts.json {
		output := struct {
			SchemaVersion int    `json:"schemaVersion"`
			ReceiptPath   string `json:"receiptPath"`
		}{
			SchemaVersion: skillinstall.SchemaVersion,
			ReceiptPath:   path,
		}
		data, err := json.Marshal(output)
		if err != nil {
			fmt.Fprintf(stderr, "pi-worker: encode skill receipt path: %v\n", err)
			return 9
		}
		fmt.Fprintln(stdout, string(data))
		return 0
	}

	fmt.Fprintln(stdout, path)
	return 0
}

func skillStatusCommand(parent context.Context, opts skillOptions, stdout, stderr io.Writer) int {
	if code, handled := skillContextExit(parent, stderr); handled {
		return code
	}

	path, err := resolveSkillReceiptPath()
	if code, handled := skillContextExit(parent, stderr); handled {
		return code
	}
	if err != nil {
		fmt.Fprintf(stderr, "pi-worker: determine skill receipt path: %v\n", err)
		return 9
	}
	if !filepath.IsAbs(path) {
		fmt.Fprintf(stderr, "pi-worker: skill receipt path must be absolute: %q\n", path)
		return 9
	}

	inspection, err := inspectSkillReceipt(path)
	if code, handled := skillContextExit(parent, stderr); handled {
		return code
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			inspection = skillinstall.Inspection{
				SchemaVersion:   skillinstall.SchemaVersion,
				ReceiptPath:     path,
				Status:          skillinstall.StatusMissing,
				VerifiedTargets: []string{},
				AffectedTargets: []skillinstall.AffectedTarget{},
				Recovery:        []string{skillinstall.SafeRecoveryCommand},
			}
		} else {
			fmt.Fprintf(stderr, "pi-worker: inspect skill status: %v\n", err)
			return 9
		}
	}
	if inspection.VerifiedTargets == nil {
		inspection.VerifiedTargets = []string{}
	}
	if inspection.AffectedTargets == nil {
		inspection.AffectedTargets = []skillinstall.AffectedTarget{}
	}
	if inspection.Recovery == nil {
		inspection.Recovery = []string{}
	}

	code := 0
	if inspection.Status != skillinstall.StatusVerified {
		code = 3
	}
	if opts.json {
		data, err := json.Marshal(inspection)
		if err != nil {
			fmt.Fprintf(stderr, "pi-worker: encode skill status: %v\n", err)
			return 9
		}
		fmt.Fprintln(stdout, string(data))
		return code
	}
	writeSkillStatusHuman(inspection, stdout)
	return code
}

func parseSkillArgs(args []string) (skillOptions, error) {
	opts := skillOptions{}
	if len(args) == 0 {
		return opts, errors.New("skill requires a subcommand")
	}
	switch args[0] {
	case "status", "receipt-path":
		opts.command = args[0]
	default:
		return opts, fmt.Errorf("unknown skill command %q", args[0])
	}

	seen := map[string]bool{}
	for i := 1; i < len(args); i++ {
		arg := args[i]
		name, _, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--json":
			if hasValue {
				return opts, fmt.Errorf("flag %s does not take a value", name)
			}
			if seen[name] {
				return opts, fmt.Errorf("flag %s specified more than once", name)
			}
			seen[name] = true
			opts.json = true
		default:
			if strings.HasPrefix(arg, "-") {
				return opts, fmt.Errorf("unknown flag %q", arg)
			}
			return opts, fmt.Errorf("unexpected argument %q", arg)
		}
	}

	return opts, nil
}

func skillContextExit(ctx context.Context, stderr io.Writer) (int, bool) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		fmt.Fprintln(stderr, "pi-worker: skill timed out")
		return 7, true
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		fmt.Fprintln(stderr, "pi-worker: skill cancelled")
		return 8, true
	}
	return 0, false
}

func writeSkillStatusHuman(inspection skillinstall.Inspection, stdout io.Writer) {
	fmt.Fprintf(stdout, "status: %s\n", inspection.Status)
	fmt.Fprintf(stdout, "receipt-path: %s\n", inspection.ReceiptPath)
	if len(inspection.VerifiedTargets) > 0 {
		fmt.Fprintln(stdout, "verified-targets:")
		for _, target := range inspection.VerifiedTargets {
			fmt.Fprintf(stdout, "- %s\n", target)
		}
	}
	if len(inspection.AffectedTargets) > 0 {
		fmt.Fprintln(stdout, "affected-targets:")
		for _, target := range inspection.AffectedTargets {
			fmt.Fprintf(stdout, "- %s (%s)\n", target.Path, target.State)
			for _, recovery := range target.Recovery {
				fmt.Fprintf(stdout, "  - %s\n", recovery)
			}
		}
	}
	if len(inspection.Recovery) > 0 {
		fmt.Fprintln(stdout, "recovery:")
		for _, recovery := range inspection.Recovery {
			fmt.Fprintf(stdout, "- %s\n", recovery)
		}
	}
}

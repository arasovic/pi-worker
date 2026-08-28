// Command piversion propagates the Pi release pin declared in
// compat/pi/package.json to every site that records it, or verifies that the
// sites already agree. Run it from the repository root.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/arasovic/pi-worker/internal/pipin"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("piversion", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	writeMode := fs.Bool("write", false, "propagate the pin to every site")
	checkMode := fs.Bool("check", false, "verify every site matches the pin")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if (*writeMode && *checkMode) || (!*writeMode && !*checkMode) || len(fs.Args()) != 0 {
		return fmt.Errorf("usage: piversion --write|--check")
	}

	root, err := os.Getwd()
	if err != nil {
		return err
	}

	if *checkMode {
		reports, err := pipin.Check(root)
		if err != nil {
			return err
		}
		for _, report := range reports {
			fmt.Println(report)
		}
		if len(reports) > 0 {
			return fmt.Errorf("%d site(s) disagree with the declared Pi pin", len(reports))
		}
		return nil
	}

	changed, err := pipin.Write(root)
	if err != nil {
		return err
	}
	if len(changed) > 0 {
		fmt.Printf("updated %d site(s) to the declared Pi pin\n", len(changed))
	}
	return nil
}

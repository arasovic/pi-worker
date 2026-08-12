package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/arasovic/pi-worker/internal/releasenotice"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("notices", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	writeMode := fs.Bool("write", false, "write notices file")
	checkMode := fs.Bool("check", false, "verify notices file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	remaining := fs.Args()
	if (*writeMode && *checkMode) || (!*writeMode && !*checkMode) || len(remaining) != 1 {
		return fmt.Errorf("usage: notices --write|--check <path>")
	}
	path := remaining[0]

	moduleCache, err := gomodcache()
	if err != nil {
		return err
	}

	if *writeMode {
		content, err := releasenotice.Render(moduleCache)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			return err
		}
		return nil
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := releasenotice.Verify(content, moduleCache); err != nil {
		return err
	}
	return nil
}

func gomodcache() (string, error) {
	cacheBytes, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		return "", err
	}
	cache := strings.TrimSpace(string(cacheBytes))
	if cache == "" {
		return "", fmt.Errorf("GOMODCACHE is empty")
	}
	return cache, nil
}

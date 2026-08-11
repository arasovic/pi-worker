package doctor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"pi-worker/internal/config"
	"pi-worker/internal/pi"
)

const maxVersionStdout = 4096
const versionWaitDelay = 100 * time.Millisecond

func DefaultDependencies(debug *pi.DebugSink) Dependencies {
	return Dependencies{
		Lookup:         exec.LookPath,
		Version:        systemVersion,
		LoadConfig:     loadUserConfig,
		CatalogFactory: pi.NewCatalog,
		Workspace:      os.Getwd,
		Debug:          debug,
	}
}

func loadUserConfig() (config.Config, error) {
	path, err := config.UserPath()
	if err != nil {
		return config.Config{}, err
	}
	return config.Load(path)
}

func systemVersion(ctx context.Context, executable string) (string, error) {
	cmd := exec.CommandContext(ctx, executable, "--version")
	cmd.WaitDelay = versionWaitDelay
	output := &boundedBuffer{limit: maxVersionStdout}
	cmd.Stdout = output
	if err := cmd.Run(); err != nil {
		return "", err
	}
	if output.exceeded {
		return "", errors.New("version output exceeds limit")
	}
	return strings.TrimSpace(output.String()), nil
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	originalLength := len(data)
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.exceeded = true
		return originalLength, nil
	}
	if len(data) > remaining {
		b.exceeded = true
		data = data[:remaining]
	}
	_, _ = b.buffer.Write(data)
	return originalLength, nil
}

func (b *boundedBuffer) Len() int { return b.buffer.Len() }

func (b *boundedBuffer) String() string { return b.buffer.String() }

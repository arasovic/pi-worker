package doctor

import (
	"context"
	"os"
	"os/exec"

	"pi-worker/internal/config"
	"pi-worker/internal/pi"
	"pi-worker/internal/piversion"
)

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
	return piversion.Probe(ctx, executable)
}

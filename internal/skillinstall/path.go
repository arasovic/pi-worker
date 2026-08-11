package skillinstall

import (
	"path/filepath"

	"pi-worker/internal/config"
)

// UserReceiptPath returns the default location of the skill-install receipt.
func UserReceiptPath() (string, error) {
	userDir, err := config.UserDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(userDir, "skill-install.json"), nil
}

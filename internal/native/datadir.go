package native

import (
	"os"
	"path/filepath"
)

// DataDirName is the directory under os.UserConfigDir() the kernel uses by default.
const DataDirName = "avora"

// DefaultDataDir is <UserConfigDir>/avora.
func DefaultDataDir() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, DataDirName), nil
}

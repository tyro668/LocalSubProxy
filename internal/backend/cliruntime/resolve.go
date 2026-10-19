package cliruntime

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func Resolve(name, label string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s CLI not found: %w", label, err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !Executable(path, info) {
		return "", fmt.Errorf("%s CLI path is not executable", label)
	}
	return path, nil
}

// ExecutableFor reports whether path (already stat'ed as info) can be run on
// goos. It is pure so both platform rules are tested on every host.
func ExecutableFor(goos, pathext, path string, info os.FileInfo) bool {
	if info.IsDir() || !info.Mode().IsRegular() {
		return false
	}
	if goos != "windows" {
		return info.Mode()&0111 != 0
	}
	// Windows has no exec bits: runnability is the extension (PATHEXT, with
	// cmd.exe's default when unset), matched case-insensitively.
	if strings.TrimSpace(pathext) == "" {
		pathext = ".COM;.EXE;.BAT;.CMD"
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return false
	}
	for _, e := range strings.Split(pathext, ";") {
		if strings.ToLower(strings.TrimSpace(e)) == ext {
			return true
		}
	}
	return false
}

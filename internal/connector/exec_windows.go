//go:build windows

package connector

import (
	"os"
	"runtime"
)

func executable(path string, info os.FileInfo) bool {
	return executableFor(runtime.GOOS, os.Getenv("PATHEXT"), path, info)
}

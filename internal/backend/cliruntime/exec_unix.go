//go:build !windows

package cliruntime

import (
	"os"
	"runtime"
)

func Executable(path string, info os.FileInfo) bool {
	return ExecutableFor(runtime.GOOS, "", path, info)
}

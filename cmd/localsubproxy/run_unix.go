//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// run replaces this process with avora, so signals, exit codes and the
// terminal behave exactly as if avora had been started directly.
func run(bin string, args []string) int {
	err := syscall.Exec(bin, append([]string{bin}, args...), os.Environ())
	fmt.Fprintln(os.Stderr, "localsubproxy: exec avora:", err)
	return 126
}

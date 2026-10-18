//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
)

// run starts avora with the same console and waits for it. Ctrl-C reaches
// both processes through the shared console; this one ignores it and lets
// avora shut down on its own.
func run(bin string, args []string) int {
	signal.Ignore(os.Interrupt)
	cmd := exec.Command(bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "localsubproxy: run avora:", err)
		return 126
	}
	return 0
}

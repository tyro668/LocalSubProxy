//go:build !linux && !darwin && !windows

package cliruntime

import (
	"os/exec"
	"time"
)

// Other platforms are not shipped: only the direct process is managed.
type tree struct{}

func (tree) prepare(*exec.Cmd)                       {}
func (tree) attach(*exec.Cmd) error                  { return nil }
func (tree) release()                                {}
func (tree) killDescendants(*exec.Cmd)               {}
func (tree) awaitExit(*exec.Cmd, time.Duration) bool { return true }
func (tree) kill(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

//go:build linux || darwin

package cliruntime

import (
	"os/exec"
	"syscall"
)

type tree struct{}

func (tree) prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}
func (tree) attach(*exec.Cmd) error { return nil }
func (tree) release()               {}

// kill signals the group led by the (unreaped) CLI process.
func (tree) kill(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// killDescendants runs once the leader has exited but before it is reaped:
// its zombie still holds the group ID, so only our survivors are signalled.
func (t tree) killDescendants(cmd *exec.Cmd) { t.kill(cmd) }

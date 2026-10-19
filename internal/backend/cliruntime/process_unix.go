//go:build !windows

package cliruntime

import (
	"os/exec"
	"syscall"
)

func SetProcessGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func KillProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
	}
}

func SetCmdLine(cmd *exec.Cmd, line string) {}

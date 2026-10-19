//go:build windows

package cliruntime

import (
	"os/exec"
	"strconv"
	"syscall"
)

func SetProcessGroup(cmd *exec.Cmd) {}
func KillProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = exec.Command("taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run()
		_ = cmd.Process.Kill()
	}
}

// SetCmdLine passes a pre-escaped command line verbatim (batch shims).
func SetCmdLine(cmd *exec.Cmd, line string) {
	if line == "" {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CmdLine = line
}

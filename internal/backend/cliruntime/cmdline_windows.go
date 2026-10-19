package cliruntime

import (
	"os/exec"
	"syscall"
)

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

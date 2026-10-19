//go:build !windows

package cliruntime

import "os/exec"

func SetCmdLine(*exec.Cmd, string) {}

package cliruntime

import (
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

// awaitExit waits until the process has exited without reaping it.
func (tree) awaitExit(cmd *exec.Cmd, limit time.Duration) bool {
	if cmd.Process == nil {
		return true
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var info unix.Siginfo
		for {
			err := unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
			if err != unix.EINTR {
				return
			}
		}
	}()
	select {
	case <-done:
		return true
	case <-time.After(limit):
		return false
	}
}

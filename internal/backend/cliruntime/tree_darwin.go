package cliruntime

import (
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

// awaitExit waits until the process has exited without reaping it
// (kqueue NOTE_EXIT leaves the zombie for Wait).
func (tree) awaitExit(cmd *exec.Cmd, limit time.Duration) bool {
	if cmd.Process == nil {
		return true
	}
	deadline := time.Now().Add(limit)
	kq, err := unix.Kqueue()
	for err == nil {
		var ev unix.Kevent_t
		unix.SetKevent(&ev, cmd.Process.Pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
		ev.Fflags = unix.NOTE_EXIT
		if _, err = unix.Kevent(kq, []unix.Kevent_t{ev}, nil, nil); err == unix.ESRCH {
			unix.Close(kq)
			return true // already a zombie
		}
		break
	}
	if err != nil { // retry a transient failure until the deadline
		if kq >= 0 {
			unix.Close(kq)
		}
		if time.Until(deadline) <= 0 {
			return false
		}
		time.Sleep(50 * time.Millisecond)
		return tree{}.awaitExit(cmd, time.Until(deadline))
	}
	defer unix.Close(kq)
	out := make([]unix.Kevent_t, 1)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return false
		}
		ts := unix.NsecToTimespec(left.Nanoseconds())
		n, err := unix.Kevent(kq, nil, out, &ts)
		if err == unix.EINTR {
			continue
		}
		return err == nil && n > 0
	}
}

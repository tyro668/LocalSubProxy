//go:build !windows

package native

import (
	"errors"
	"os"
	"syscall"
)

var errWouldBlock = errors.New("lock held")

func tryLock(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errWouldBlock
	}
	return err
}

func unlock(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

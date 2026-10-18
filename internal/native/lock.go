package native

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// lockFile guards a data directory: only one kernel (the desktop app or
// `avora serve`) may run against it, because both rewrite
// identity.json, the key files and adapters.enc. The OS releases the lock
// when the process exits, so a crash never leaves a stale lock behind.
const lockFile = "kernel.lock"

// ErrDataDirInUse is returned by start when another process holds the lock.
var ErrDataDirInUse = errors.New("data directory in use")

type dirLock struct{ f *os.File }

func lockDir(dir string) (*dirLock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := tryLock(f); err != nil {
		_ = f.Close()
		if errors.Is(err, errWouldBlock) {
			return nil, fmt.Errorf("%w: %s is used by another Avora process (the desktop app or \"avora serve\"); stop it first", ErrDataDirInUse, dir)
		}
		return nil, err
	}
	// pid for humans; the lock itself is what counts
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0)
	return &dirLock{f: f}, nil
}

func (l *dirLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = unlock(l.f)
	_ = l.f.Close()
	l.f = nil
}

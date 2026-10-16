//go:build !windows

package control

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// SocketName is the control socket inside the data directory.
const SocketName = "control.sock"

// Address is the socket path for dataDir.
func Address(dataDir string) string { return filepath.Join(dataDir, SocketName) }

// Listen creates the control socket, readable and writable by the owner
// only. A leftover socket file from a crashed serve is replaced; a live one
// is an error (the data directory lock normally catches that first).
func Listen(dataDir string) (net.Listener, error) {
	path := Address(dataDir)
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		_ = c.Close()
		return nil, fmt.Errorf("control socket %s is already served by another process", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	old := syscall.Umask(0o177) // the socket is created 0600, no window
	l, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}

func dial(dataDir string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", Address(dataDir), timeout)
}

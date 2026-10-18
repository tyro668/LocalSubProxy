//go:build windows

package control

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// Address is the named pipe for dataDir: one pipe per data directory.
func Address(dataDir string) string {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		abs = dataDir
	}
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(abs))))
	return `\\.\pipe\avora-` + hex.EncodeToString(sum[:8])
}

// PipeSDDL grants full access to the given user SID only (no inherited
// ACEs, so not Everyone, not other users, not anonymous).
func PipeSDDL(sid string) string { return "D:P(A;;GA;;;" + sid + ")" }

func currentUserSID() (string, error) {
	tok := windows.GetCurrentProcessToken()
	u, err := tok.GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
}

// Listen creates the pipe, restricted to the current user. The first
// instance flag makes a second serve (or a squatter) fail instead of
// sharing the name.
func Listen(dataDir string) (net.Listener, error) {
	sid, err := currentUserSID()
	if err != nil {
		return nil, fmt.Errorf("current user: %w", err)
	}
	return winio.ListenPipe(Address(dataDir), &winio.PipeConfig{
		SecurityDescriptor: PipeSDDL(sid),
		InputBufferSize:    64 << 10,
		OutputBufferSize:   64 << 10,
	})
}

func dial(dataDir string, timeout time.Duration) (net.Conn, error) {
	return winio.DialPipe(Address(dataDir), &timeout)
}

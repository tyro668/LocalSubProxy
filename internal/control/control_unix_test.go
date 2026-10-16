//go:build !windows

package control

import (
	"os"
	"testing"
)

// short paths: unix socket paths are limited to ~104 bytes on macOS
func shortDir(t *testing.T) string {
	d, err := os.MkdirTemp("", "lspc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func TestSocketIsOwnerOnly(t *testing.T) {
	dir := shortDir(t)
	startServer(t, dir, func(string) string { return `{"ok":true}` })
	st, err := os.Stat(Address(dir))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSocket == 0 || st.Mode().Perm() != 0o600 {
		t.Fatalf("control socket mode %v, want socket 0600", st.Mode())
	}
}

func TestStaleSocketReplaced(t *testing.T) {
	dir := shortDir(t)
	l, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	// simulate a crash: the file stays, nobody listens
	if ul, ok := l.(interface{ SetUnlinkOnClose(bool) }); ok {
		ul.SetUnlinkOnClose(false)
	}
	_ = l.Close()
	if _, err := os.Stat(Address(dir)); err != nil {
		t.Fatalf("stale socket file not left behind: %v", err)
	}
	l2, err := Listen(dir)
	if err != nil {
		t.Fatalf("stale socket must be replaced: %v", err)
	}
	_ = l2.Close()
}

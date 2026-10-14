//go:build windows

package connector

import (
	"path/filepath"
	"testing"
)

// On a real Windows host: a codex.cmd shim on PATH resolves.
func TestResolveCodexWindowsShim(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "codex.cmd", 0o644)
	t.Setenv("PATH", dir)
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
	got, err := resolveCodex("codex")
	if err != nil {
		t.Fatalf("resolveCodex: %v", err)
	}
	if filepath.Base(got) != "codex.cmd" {
		t.Fatalf("resolved %s", got)
	}
}

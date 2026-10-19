//go:build windows

package cliruntime

import (
	"path/filepath"
	"testing"
)

// On a real Windows host: a codex.cmd shim on PATH resolves.
func TestResolveWindowsShim(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "codex.cmd", 0o644)
	t.Setenv("PATH", dir)
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
	got, err := Resolve("codex", "Codex")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if filepath.Base(got) != "codex.cmd" {
		t.Fatalf("resolved %s", got)
	}
}

package connector

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func writeFile(t *testing.T, dir, name string, mode os.FileMode) (string, os.FileInfo) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("x"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return p, info
}

// Windows has no exec bits (Go reports 0666/0444), so the Unix 0111 rule
// rejected every real codex.exe / codex.cmd. Windows MUST decide by extension
// (PATHEXT, falling back to the cmd.exe default) on a regular file.
func TestExecutableForWindows(t *testing.T) {
	dir := t.TempDir()
	const pathext = ".COM;.EXE;.BAT;.CMD"
	for _, c := range []struct {
		name, pathext string
		want          bool
	}{
		{"codex.exe", pathext, true},
		{"codex.CMD", pathext, true},
		{"codex.ps1", pathext, false},
		{"codex", pathext, false},
		{"codex.txt", pathext, false},
		{"codex.exe", "", true}, // empty PATHEXT: default list
		{"codex.cmd", " .Cmd ;", true},
	} {
		p, info := writeFile(t, dir, c.name, 0o644) // no exec bits, like Windows
		if got := executableFor("windows", c.pathext, p, info); got != c.want {
			t.Errorf("windows %s (PATHEXT %q) = %v, want %v", c.name, c.pathext, got, c.want)
		}
	}
	sub := filepath.Join(dir, "codex.exe.d.exe")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(sub)
	if executableFor("windows", pathext, sub, info) {
		t.Error("a directory named *.exe must not count as executable")
	}
}

func TestExecutableForUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod exec bits are not representable on Windows")
	}
	dir := t.TempDir()
	p, info := writeFile(t, dir, "codex", 0o755)
	if !executableFor("linux", "", p, info) {
		t.Error("0755 file must be executable on unix")
	}
	p, info = writeFile(t, dir, "codex.exe", 0o644)
	if executableFor("darwin", ".EXE", p, info) {
		t.Error("unix ignores extensions: 0644 codex.exe is not executable")
	}
	if executableFor("linux", "", dir, mustStat(t, dir)) {
		t.Error("directory must not be executable")
	}
}

func mustStat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The shim forwards arguments and the exit code to the avora binary next
// to it and prints the rename notice on stderr.
func TestShimForwardsToAvora(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries")
	}
	dir := t.TempDir()
	exe := func(n string) string {
		if runtime.GOOS == "windows" {
			return n + ".exe"
		}
		return n
	}
	// a stand-in avora: prints its args, exits 3
	src := filepath.Join(dir, "fake")
	_ = os.MkdirAll(src, 0o755)
	_ = os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\nimport (\"fmt\";\"os\";\"strings\")\nfunc main(){fmt.Print(\"avora:\"+strings.Join(os.Args[1:],\",\"));os.Exit(3)}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(src, "go.mod"), []byte("module fake\ngo 1.22\n"), 0o644)
	build := exec.Command("go", "build", "-o", filepath.Join(dir, exe("avora")), ".")
	build.Dir = src
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake avora: %v\n%s", err, out)
	}
	shim := filepath.Join(dir, exe("localsubproxy"))
	if out, err := exec.Command("go", "build", "-o", shim, ".").CombinedOutput(); err != nil {
		t.Fatalf("build shim: %v\n%s", err, out)
	}
	cmd := exec.Command(shim, "status", "--json")
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 3 {
		t.Fatalf("exit: %v", err)
	}
	if stdout.String() != "avora:status,--json" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "renamed to avora") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

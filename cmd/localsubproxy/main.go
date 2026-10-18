// Command localsubproxy is the pre-rename command name of avora, shipped in
// the CLI archives for one release so existing scripts and service units
// keep working. It prints a deprecation notice and runs the avora binary
// next to it (or the one on PATH) with the same arguments. Remove it in the
// release after the rename.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

const notice = "localsubproxy has been renamed to avora; this forwarding command will be removed in the next release, run avora instead"

func target() (string, error) {
	name := "avora"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if self, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(self); err == nil {
			self = real
		}
		p := filepath.Join(filepath.Dir(self), name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return exec.LookPath("avora")
}

func main() {
	fmt.Fprintln(os.Stderr, "notice: "+notice)
	bin, err := target()
	if err != nil {
		fmt.Fprintln(os.Stderr, "localsubproxy: avora not found next to this command or on PATH:", err)
		os.Exit(127)
	}
	os.Exit(run(bin, os.Args[1:]))
}

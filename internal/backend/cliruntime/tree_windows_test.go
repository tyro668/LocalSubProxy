package cliruntime

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Negative control for the previous strategy: once a CLI has exited,
// `taskkill /PID <pid> /T /F` cannot see its orphaned grandchildren (and the
// PID may already belong to someone else). The Job Object tests above pass
// where this one shows the old approach leaking.
func TestTaskkillTreeMissesOrphans(t *testing.T) {
	o, beat := treeOptions(t, "exit")
	cmd := exec.CommandContext(context.Background(), o.Executable)
	cmd.Env = o.Env
	cmd.Dir = o.Dir
	if out, err := cmd.Output(); err != nil || strings.TrimSpace(string(out)) != "ready" {
		t.Fatalf("child: %q %v", out, err)
	}
	_ = exec.Command("taskkill", "/PID", strconv.Itoa(cmd.ProcessState.Pid()), "/T", "/F").Run()
	time.Sleep(100 * time.Millisecond)
	if !alive(beat) {
		t.Fatal("expected the orphaned grandchild to survive taskkill /T")
	}
	b, err := os.ReadFile(beat + ".pid")
	if err != nil {
		t.Fatal(err)
	}
	if pid, err := strconv.Atoi(string(b)); err == nil {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	}
}

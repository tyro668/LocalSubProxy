package main

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// fakeCodex writes an App Server stand-in that answers initialize at once
// but holds account/login/start until gate exists; it records the start in
// started and its pid in exited (pidfile).
func fakeCodex(t *testing.T) (bin, gate, started, exited string) {
	t.Helper()
	d := t.TempDir()
	bin, gate = filepath.Join(d, "codex"), filepath.Join(d, "gate")
	started, exited = filepath.Join(d, "started"), filepath.Join(d, "exited")
	script := `#!/usr/bin/env python3
import sys, json, os, time
open(` + "'" + exited + "'" + `, 'w').write(str(os.getpid()))
for line in sys.stdin:
    m = json.loads(line)
    if m.get('method') == 'initialize':
        print(json.dumps({'id': m['id'], 'result': {}}), flush=True)
    elif m.get('method') == 'account/login/start':
        open(` + "'" + started + "'" + `, 'w').close()
        while not os.path.exists(` + "'" + gate + "'" + `):
            time.sleep(0.02)
        print(json.dumps({'id': m['id'], 'result': {'type': 'chatgpt', 'loginId': 'L1', 'authUrl': 'https://auth.example/x'}}), flush=True)
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return
}

func waitFile(t *testing.T, p string) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if _, err := os.Stat(p); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", p)
}

// Review8 P2-b: cancel / logout while BeginCodexLogin is still running must
// win. The late start result is closed and discarded, never installed.
func TestCodexLoginCancelDuringStartWins(t *testing.T) {
	for _, action := range []string{"codex_login_cancel", "codex_logout"} {
		t.Run(action, func(t *testing.T) {
			startKernel(t, t.TempDir())
			bin, gate, started, exited := fakeCodex(t)
			res := make(chan reply, 1)
			go func() { res <- do(t, map[string]any{"action": "codex_login_start", "path": bin}) }()
			waitFile(t, started) // the real start is now inside BeginCodexLogin
			r := do(t, map[string]any{"action": action, "path": "/nonexistent-codex"})
			if action == "codex_login_cancel" && !r.OK {
				t.Fatalf("cancel failed: %s", r.Error)
			}
			os.WriteFile(gate, nil, 0o600) // let the stale start return
			sr := <-res
			if sr.OK {
				t.Fatalf("stale start reported success: %+v", sr.Data)
			}
			runtime.Lock()
			l := runtime.login
			runtime.Unlock()
			if l != nil {
				t.Fatal("stale login session was installed after " + action)
			}
			waitGone(t, exited) // its App Server was shut down
			if r := do(t, map[string]any{"action": "codex_login_check"}); r.OK {
				t.Fatal("check found a login after cancel")
			}
		})
	}
}

// Control: an uncontested start still installs its session.
func TestCodexLoginStartInstalls(t *testing.T) {
	startKernel(t, t.TempDir())
	bin, gate, _, _ := fakeCodex(t)
	os.WriteFile(gate, nil, 0o600)
	d := must(t, map[string]any{"action": "codex_login_start", "path": bin})
	if d["auth_url"] != "https://auth.example/x" {
		t.Fatalf("auth_url %v", d["auth_url"])
	}
	runtime.Lock()
	ok := runtime.login != nil
	runtime.Unlock()
	if !ok {
		t.Fatal("login not installed")
	}
}

func waitGone(t *testing.T, pidfile string) {
	t.Helper()
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(string(b))
	for i := 0; i < 500; i++ {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("stale App Server pid %d still running", pid)
}

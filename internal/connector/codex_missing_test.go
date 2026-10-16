package connector

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func copyExec(t *testing.T, dst string) {
	t.Helper()
	src, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, src); err != nil {
		t.Fatal(err)
	}
	out.Close()
}

// Review9 P2-4: a saved Codex scheme whose CLI was later removed, moved or
// lost its exec permission made the whole adapter file fail to load, even
// when the scheme was disabled: the kernel did not start, every other scheme
// and SaaS connection went down, and the broken scheme could not be edited
// or deleted. Loading must keep the scheme, report the missing CLI as its
// health, and allow editing and deleting it.
func TestUnavailableCodexCLIDoesNotBlockLoad(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		breakIt func(path string) error
	}{
		{"removed", true, os.Remove},
		{"no-exec-permission", true, func(p string) error { return os.Chmod(p, 0o644) }},
		{"removed-disabled", false, os.Remove},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "no-exec-permission" && runtime.GOOS == "windows" {
				t.Skip("Windows has no POSIX execute bit")
			}
			dir := t.TempDir()
			bin := filepath.Join(dir, "codex")
			if runtime.GOOS == "windows" {
				bin += ".exe"
			}
			copyExec(t, bin)
			file := filepath.Join(dir, "adapters.enc")
			x, err := New(Config{DataKey: codexTestDataKey, AdapterFile: file, MaxConcurrency: 4})
			if err != nil {
				t.Fatal(err)
			}
			ok := AdapterConfig{ID: "lm", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", APIKey: "k", Models: []string{"m1"}, Enabled: true, SkipModelProbe: true}
			if _, err := x.CreateAdapter(ok); err != nil {
				t.Fatal(err)
			}
			cx := AdapterConfig{ID: "codex", Type: "codex_app_server", CodexBin: bin, Models: []string{"fake-model"}, Enabled: tc.enabled}
			if _, err := x.CreateAdapter(cx); err != nil {
				t.Fatal(err)
			}
			x.Shutdown()
			if err := tc.breakIt(bin); err != nil {
				t.Fatal(err)
			}

			y, err := New(Config{DataKey: codexTestDataKey, AdapterFile: file, MaxConcurrency: 4})
			if err != nil {
				t.Fatalf("kernel failed to start because of one Codex scheme: %v", err)
			}
			defer y.Shutdown()
			if n := len(y.ListAdapters()); n != 2 {
				t.Fatalf("schemes after restart: %d", n)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			y.mu.Lock()
			a := y.adapters["codex"]
			y.mu.Unlock()
			h := y.probeAdapter(ctx, a)
			want := "unavailable"
			if !tc.enabled {
				want = "disabled"
			}
			if h.Health != want {
				t.Fatalf("health %+v, want %s", h, want)
			}
			if tc.enabled && !strings.Contains(h.Error, "Codex CLI") {
				t.Fatalf("health does not say why: %+v", h)
			}
			// the other scheme is unaffected
			y.mu.Lock()
			other := y.adapters["lm"]
			y.mu.Unlock()
			if hh := y.probeAdapter(ctx, other); hh.Health != "ready" {
				t.Fatalf("other scheme %+v", hh)
			}
			// editable: rename and disable without a working CLI
			if _, err := y.PatchAdapter("codex", json.RawMessage(`{"name":"old codex","enabled":false}`)); err != nil {
				t.Fatalf("edit unavailable Codex scheme: %v", err)
			}
			// enabling it again needs a working CLI: a clear error, nothing saved
			if _, err := y.PatchAdapter("codex", json.RawMessage(`{"enabled":true}`)); err == nil || !strings.Contains(err.Error(), "Codex CLI") {
				t.Fatalf("enable without CLI: %v", err)
			}
			if err := y.DeleteAdapter("codex"); err != nil {
				t.Fatalf("delete unavailable Codex scheme: %v", err)
			}
			if n := len(y.ListAdapters()); n != 1 {
				t.Fatalf("schemes after delete: %d", n)
			}
		})
	}
}

const codexTestDataKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

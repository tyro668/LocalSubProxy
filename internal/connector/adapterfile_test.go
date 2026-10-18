package connector

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyro668/avora/internal/durable"
)

const (
	keyA = "1111111111111111111111111111111111111111111111111111111111111111"
	keyB = "2222222222222222222222222222222222222222222222222222222222222222"
)

func sealAdapters(t *testing.T, path, key string, ads []AdapterConfig) {
	t.Helper()
	c, err := durable.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Seal(ads)
	if err != nil {
		t.Fatal(err)
	}
	if err := durable.Write(path, b); err != nil {
		t.Fatal(err)
	}
}

var oneAdapter = []AdapterConfig{{ID: "oa", Type: "openai_compatible", Name: "OpenAI", BaseURL: "https://api.example/v1", APIKey: "sk-x", Models: []string{"gpt-4o"}, Enabled: true}}

func backups(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, "adapters.enc.bak-*"))
	return m
}

// A file sealed with a legacy key opens through LegacyDataKeys and is
// re-encrypted with DataKey (migration), so the next start needs no legacy key.
func TestAdapterFileLegacyKeyMigrates(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "adapters.enc")
	sealAdapters(t, f, keyA, oneAdapter)
	x, err := New(Config{DataKey: keyB, LegacyDataKeys: []string{keyA}, AdapterFile: f})
	if err != nil {
		t.Fatalf("legacy-key file must open: %v", err)
	}
	if got := x.ListAdapters(); len(got) != 1 || x.adapters["oa"].APIKey != "sk-x" {
		t.Fatalf("adapters %+v", got)
	}
	if _, ok := x.Status()["adapter_recovery"]; ok {
		t.Fatal("successful migration must not report a recovery")
	}
	y, err := New(Config{DataKey: keyB, AdapterFile: f})
	if err != nil || len(y.ListAdapters()) != 1 {
		t.Fatalf("file was not re-encrypted with the data key: %v", err)
	}
	if len(backups(t, dir)) != 0 {
		t.Fatal("no backup expected on a successful migration")
	}
}

// Undecryptable (wrong key) or corrupt files must not brick startup: back
// the file up, start empty, and report it for the UI.
func TestAdapterFileUnreadableIsBackedUp(t *testing.T) {
	for name, write := range map[string]func(t *testing.T, f string){
		"wrong key": func(t *testing.T, f string) { sealAdapters(t, f, keyA, oneAdapter) },
		"too short": func(t *testing.T, f string) { _ = os.WriteFile(f, []byte("abc"), 0o600) },
		"garbage":   func(t *testing.T, f string) { _ = os.WriteFile(f, []byte(strings.Repeat("\x00garbage", 40)), 0o600) },
		"not adapter": func(t *testing.T, f string) {
			c, _ := durable.NewCipher(keyB)
			b, _ := c.Seal("x")
			_ = os.WriteFile(f, b, 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			f := filepath.Join(dir, "adapters.enc")
			write(t, f)
			orig, _ := os.ReadFile(f)
			x, err := New(Config{DataKey: keyB, AdapterFile: f})
			if err != nil {
				t.Fatalf("startup must not fail: %v", err)
			}
			if len(x.ListAdapters()) != 0 {
				t.Fatal("expected an empty adapter list")
			}
			bk := backups(t, dir)
			if len(bk) != 1 {
				t.Fatalf("backups %v", bk)
			}
			if b, _ := os.ReadFile(bk[0]); string(b) != string(orig) {
				t.Fatal("backup differs from the original file")
			}
			// the unreadable file is replaced by an empty list sealed with the
			// current key, so the next start does not back up again
			if b, _ := os.ReadFile(f); string(b) == string(orig) {
				t.Fatal("unreadable file must be moved aside")
			}
			if saved, migrated, err := openAdapters(mustRead(t, f), x.cipher, nil); err != nil || migrated || len(saved) != 0 {
				t.Fatalf("adapters.enc must be an empty list under the current key: %v %v", saved, err)
			}
			rec, ok := x.Status()["adapter_recovery"].(map[string]any)
			if !ok || rec["backup"] != bk[0] || rec["reason"] == "" {
				t.Fatalf("recovery status %v", x.Status()["adapter_recovery"])
			}
			// the user can add adapters again and they persist
			if _, err := x.CreateAdapter(oneAdapter[0]); err != nil {
				t.Fatal(err)
			}
			y, err := New(Config{DataKey: keyB, AdapterFile: f})
			if err != nil || len(y.ListAdapters()) != 1 {
				t.Fatalf("re-added adapter did not persist: %v", err)
			}
		})
	}
}

func mustRead(t *testing.T, f string) []byte {
	t.Helper()
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The recovery notice is persisted (it survives restarts) until the user
// acknowledges it, is never shown again afterwards, and restarts never
// produce another backup. New adapters are sealed with the current key.
func TestAdapterRecoveryNoticeAcknowledged(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "adapters.enc")
	sealAdapters(t, f, keyA, oneAdapter)
	x, err := New(Config{DataKey: keyB, AdapterFile: f})
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := x.Status()["adapter_recovery"].(map[string]any)
	if !ok || rec["backup"] == "" {
		t.Fatalf("expected a recovery notice, got %v", x.Status()["adapter_recovery"])
	}
	// restart before acknowledging: same notice, still exactly one backup
	y, err := New(Config{DataKey: keyB, AdapterFile: f})
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := y.Status()["adapter_recovery"].(map[string]any); !ok || r["backup"] != rec["backup"] {
		t.Fatalf("notice must persist until acknowledged: %v", y.Status()["adapter_recovery"])
	}
	if n := len(backups(t, dir)); n != 1 {
		t.Fatalf("restarts must not create more backups, got %d", n)
	}
	if err := y.AcknowledgeAdapterRecovery(); err != nil {
		t.Fatal(err)
	}
	if _, ok := y.Status()["adapter_recovery"]; ok {
		t.Fatal("acknowledged notice must disappear immediately")
	}
	if err := y.AcknowledgeAdapterRecovery(); err != nil {
		t.Fatalf("acknowledging twice must be harmless: %v", err)
	}
	// the app is usable: add an adapter, it is sealed with the current key
	if _, err := y.CreateAdapter(oneAdapter[0]); err != nil {
		t.Fatal(err)
	}
	if saved, _, err := openAdapters(mustRead(t, f), y.cipher, nil); err != nil || len(saved) != 1 {
		t.Fatalf("new adapter must be sealed with the current key: %v", err)
	}
	z, err := New(Config{DataKey: keyB, AdapterFile: f})
	if err != nil || len(z.ListAdapters()) != 1 {
		t.Fatalf("restart after ack: %v", err)
	}
	if _, ok := z.Status()["adapter_recovery"]; ok {
		t.Fatal("acknowledged notice must not come back after a restart")
	}
	if n := len(backups(t, dir)); n != 1 {
		t.Fatalf("backups %d", n)
	}
}

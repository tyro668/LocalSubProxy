package native

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"
)

// configRoot points os.UserConfigDir() at a temp directory.
func configRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	switch goruntime.GOOS {
	case "windows":
		t.Setenv("APPDATA", root)
	case "darwin", "ios":
		t.Setenv("HOME", root)
	default:
		t.Setenv("XDG_CONFIG_HOME", root)
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func startDefault(t *testing.T) reply {
	t.Helper()
	var r reply
	_ = json.Unmarshal([]byte(start("")), &r)
	if r.OK {
		t.Cleanup(func() { call(`{"action":"stop"}`) })
	}
	return r
}

func waitConnected(t *testing.T, id string) map[string]any {
	t.Helper()
	var last map[string]any
	for i := 0; i < 100; i++ {
		for _, c := range conns(t) {
			m := c.(map[string]any)
			if m["id"] == id {
				last = m
				if m["connected"] == true {
					return m
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("connection %s never came online: %v", id, last)
	return nil
}

// Rename regression: a device paired by LocalSubProxy (data under
// <UserConfigDir>/localsubproxy) upgrades to Avora. The first start with the
// default directory moves everything to <UserConfigDir>/avora, keeps the old
// directory as a backup, and the existing connection comes back online
// without pairing again.
func TestLegacyDataDirMigratedWithoutRepair(t *testing.T) {
	cfg := configRoot(t)
	s := newTestSaaS(t, "acme")
	oldDir := filepath.Join(cfg, "localsubproxy")
	newDir := filepath.Join(cfg, "avora")

	// the pre-rename app pairs once
	startKernel(t, oldDir)
	st := pairOnce(t, s, []string{})
	if st["state"] != "paired" {
		t.Fatalf("pairing: %v", st)
	}
	cs := conns(t)
	if len(cs) != 1 {
		t.Fatalf("connections after pairing: %v", cs)
	}
	before := cs[0].(map[string]any)
	id := before["id"].(string)
	waitConnected(t, id)
	must(t, map[string]any{"action": "stop"})

	// first start of the renamed app, default data directory
	r := startDefault(t)
	if !r.OK {
		t.Fatalf("start with default data dir: %s", r.Error)
	}
	after := waitConnected(t, id)
	if after["device_id"] != before["device_id"] || len(conns(t)) != 1 {
		t.Fatalf("connection changed: before %v after %v", before, after)
	}
	if _, err := os.Stat(filepath.Join(newDir, "identity.json")); err != nil {
		t.Fatalf("identity.json not in the new data dir: %v", err)
	}
	if ents, _ := os.ReadDir(filepath.Join(newDir, "bindings")); len(ents) != 1 {
		t.Fatalf("binding keys not migrated: %d", len(ents))
	}
	if _, err := os.Stat(oldDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old data dir still active: %v", err)
	}
	backups, _ := filepath.Glob(filepath.Join(cfg, "localsubproxy.backup-*"))
	if len(backups) != 1 {
		t.Fatalf("backups: %v", backups)
	}
	if _, err := os.Stat(filepath.Join(backups[0], "identity.json")); err != nil {
		t.Fatalf("backup incomplete: %v", err)
	}
	mig, _ := must(t, map[string]any{"action": "status"})["data_dir_migration"].(map[string]any)
	if mig["from"] != oldDir || mig["backup"] != backups[0] {
		t.Fatalf("status data_dir_migration = %v", mig)
	}

	// a later restart does not migrate again
	must(t, map[string]any{"action": "stop"})
	if r := startDefault(t); !r.OK {
		t.Fatal(r.Error)
	}
	waitConnected(t, id)
	if b, _ := filepath.Glob(filepath.Join(cfg, "localsubproxy.backup-*")); len(b) != 1 {
		t.Fatalf("migrated twice: %v", b)
	}
}

// The old app still runs against the old directory: nothing is copied and
// start explains that the old app must be quit first.
func TestLegacyDataDirInUseIsNotMigrated(t *testing.T) {
	cfg := configRoot(t)
	oldDir := filepath.Join(cfg, "localsubproxy")
	l, err := lockDir(oldDir)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(oldDir, "identity.json"), []byte(`{"version":2}`), 0o600)
	r := startDefault(t)
	l.release()
	if r.OK || !strings.Contains(r.Error, "LocalSubProxy") {
		t.Fatalf("start while the old app runs: ok=%v err=%q", r.OK, r.Error)
	}
	if _, err := os.Stat(filepath.Join(cfg, "avora", "identity.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live directory was copied: %v", err)
	}
}

func TestMigrateLegacyEdgeCases(t *testing.T) {
	root := t.TempDir()
	oldDir, newDir := filepath.Join(root, "localsubproxy"), filepath.Join(root, "avora")
	write := func(p, s string) {
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(oldDir, "identity.json"), "old")
	write(filepath.Join(oldDir, "bindings", "b1", "device_key.enc"), "key")
	write(filepath.Join(oldDir, lockFile), "123\n") // stale lock from a crash
	// leftover stage of an interrupted migration
	write(filepath.Join(newDir+".migrating", "identity.json"), "partial")

	m, err := migrateLegacy(newDir, oldDir)
	if err != nil || m.From != oldDir || m.Backup == "" {
		t.Fatalf("migrate: %+v %v", m, err)
	}
	if b, _ := os.ReadFile(filepath.Join(newDir, "identity.json")); string(b) != "old" {
		t.Fatalf("identity.json = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(newDir, "bindings", "b1", "device_key.enc")); string(b) != "key" {
		t.Fatalf("binding key = %q", b)
	}
	if _, err := os.Stat(filepath.Join(newDir, lockFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale kernel.lock copied")
	}
	if _, err := os.Stat(newDir + ".migrating"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stage left behind")
	}
	if goruntime.GOOS != "windows" {
		if fi, _ := os.Stat(filepath.Join(newDir, "bindings", "b1", "device_key.enc")); fi.Mode().Perm() != 0o600 {
			t.Fatalf("key file mode %v", fi.Mode().Perm())
		}
	}
	// the new directory already has data: never overwritten
	write(filepath.Join(oldDir, "identity.json"), "newer-old")
	if m, err := migrateLegacy(newDir, oldDir); err != nil || m.From != "" {
		t.Fatalf("second migrate: %+v %v", m, err)
	}
	if b, _ := os.ReadFile(filepath.Join(newDir, "identity.json")); string(b) != "old" {
		t.Fatalf("overwrote the new data dir: %q", b)
	}
	// an explicit (non-default) data dir is never migrated into
	if m, err := MigrateDefaultDataDir(filepath.Join(root, "custom")); err != nil || m.From != "" {
		t.Fatalf("custom dir: %+v %v", m, err)
	}
}

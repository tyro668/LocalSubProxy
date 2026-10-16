package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// testdata/upstream was written by the upstream app's own code (dda2be5):
// identity.json has only journal_key and adapters.enc is sealed with it.
func copyFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// the fixture has a Codex adapter: put a fake codex CLI on PATH
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	for _, n := range []string{"identity.json", "adapters.enc"} {
		b, err := os.ReadFile(filepath.Join("testdata", "upstream", n))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, n), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const upstreamKey = "1f2e3d4c5b6a79881f2e3d4c5b6a79881f2e3d4c5b6a79881f2e3d4c5b6a7988"

func TestUpstreamIdentityKeepsKeyAndOpensAdapters(t *testing.T) {
	dir := copyFixture(t)
	id, err := loadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if id.DataKey != upstreamKey {
		t.Fatalf("data key %q: the upstream journal_key must be adopted, not replaced", id.DataKey)
	}
	var onDisk map[string]any
	b, _ := os.ReadFile(filepath.Join(dir, "identity.json"))
	_ = json.Unmarshal(b, &onDisk)
	if onDisk["journal_key"] != upstreamKey || onDisk["data_key"] != upstreamKey || onDisk["remote_url"] != "https://saas.example" {
		t.Fatalf("identity.json lost fields: %v", onDisk)
	}
	core, err := newCore(dir, id)
	if err != nil {
		t.Fatalf("upstream adapters.enc must open: %v", err)
	}
	if n := len(core.ListAdapters()); n != 2 {
		t.Fatalf("adapters %d, want 2", n)
	}
	if _, bad := core.Status()["adapter_recovery"]; bad {
		t.Fatal("unexpected recovery")
	}
}

// The state our earlier build left behind: identity.json rewritten with a
// fresh data_key. Restoring the old journal_key next to it recovers and
// migrates the adapters.
func TestRestoredJournalKeyMigratesToDataKey(t *testing.T) {
	dir := copyFixture(t)
	newKey := "9999999999999999999999999999999999999999999999999999999999999999"
	b, _ := json.Marshal(map[string]string{"data_key": newKey, "journal_key": upstreamKey})
	_ = os.WriteFile(filepath.Join(dir, "identity.json"), b, 0o600)
	id, err := loadIdentity(dir)
	if err != nil || id.DataKey != newKey {
		t.Fatalf("%v %q", err, id.DataKey)
	}
	core, err := newCore(dir, id)
	if err != nil || len(core.ListAdapters()) != 2 {
		t.Fatalf("legacy key not tried: %v %v %d", err, core.Status()["adapter_recovery"], len(core.ListAdapters()))
	}
	only := localIdentity{DataKey: newKey}
	core2, err := newCore(dir, only)
	if err != nil || len(core2.ListAdapters()) != 2 {
		t.Fatalf("adapters.enc was not re-encrypted with data_key: %v", err)
	}
}

func TestLostKeyDoesNotBrickStartup(t *testing.T) {
	dir := copyFixture(t)
	_ = os.WriteFile(filepath.Join(dir, "identity.json"), []byte(`{"data_key":"9999999999999999999999999999999999999999999999999999999999999999"}`), 0o600)
	id, _ := loadIdentity(dir)
	core, err := newCore(dir, id)
	if err != nil {
		t.Fatalf("startup failed: %v", err)
	}
	if rec, ok := core.Status()["adapter_recovery"].(map[string]any); !ok || rec["backup"] == "" {
		t.Fatalf("no recovery reported: %v", core.Status()["adapter_recovery"])
	}
}

func TestFreshInstallGeneratesKeyOnce(t *testing.T) {
	dir := t.TempDir()
	a, err := loadIdentity(dir)
	if err != nil || len(a.DataKey) != 64 {
		t.Fatal(err, a.DataKey)
	}
	b, _ := loadIdentity(dir)
	if b.DataKey != a.DataKey {
		t.Fatal("key regenerated on second start")
	}
}

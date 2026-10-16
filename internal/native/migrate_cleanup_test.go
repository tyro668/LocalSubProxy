package native

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Review7 #16: when the original v1 device_key.enc cannot be removed after
// migration, it is persisted in pending_cleanup and removed by the next
// start's cleanup retry instead of lingering forever.
func TestMigrateV1OriginalKeyRemoveFailureIsPersisted(t *testing.T) {
	dir := t.TempDir()
	v1 := localIdentity{DataKey: strings.Repeat("ab", 32), DeviceID: "dev_1", RemoteURL: "https://saas.example"}
	if err := saveDevice(dir, v1, fakeIdentity("https://gw.example", "acme", "dev_1")); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v1)
	_ = os.WriteFile(filepath.Join(dir, "identity.json"), b, 0o600)
	orig := removeOne
	removeOne = func(p string) error {
		if filepath.Base(p) == deviceKeyFile {
			return errors.New("sharing violation (injected)")
		}
		return orig(p)
	}
	id, err := loadIdentity(dir)
	removeOne = orig
	if err != nil || len(id.Bindings) != 1 {
		t.Fatalf("migration: %+v %v", id, err)
	}
	if !slices.Contains(onDisk(t, dir).PendingCleanup, deviceKeyFile) {
		t.Fatalf("original key not recorded for cleanup: %v", onDisk(t, dir).PendingCleanup)
	}
	if _, err := os.Stat(filepath.Join(dir, deviceKeyFile)); err != nil {
		t.Fatal("precondition: the original key is still there")
	}
	left, errs := runCleanup(dir, onDisk(t, dir).PendingCleanup)
	if len(left) != 0 || len(errs) != 0 {
		t.Fatalf("retry: %v %v", left, errs)
	}
	if _, err := os.Stat(filepath.Join(dir, deviceKeyFile)); !os.IsNotExist(err) {
		t.Fatal("original v1 key still on disk after cleanup retry")
	}
	// a revoke of the migrated binding also lists the original key
	if p := bindingCleanupPaths(id.Bindings[0]); !slices.Contains(p, deviceKeyFile) {
		t.Fatalf("revoke cleanup misses original key: %v", p)
	}
}

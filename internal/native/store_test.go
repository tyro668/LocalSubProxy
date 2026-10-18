package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyro668/avora/internal/lmbp"
	"github.com/tyro668/avora/internal/lmbp/client"
)

func fakeIdentity(gw, tenant, dev string) client.Identity {
	x, _ := lmbp.NewX25519()
	return client.Identity{GatewayURL: gw, Tenant: tenant, DeviceID: dev, PrivateKey: lmbp.B64(make([]byte, 64)), GatewayKey: lmbp.B64(make([]byte, 32)),
		E2EEKey: lmbp.B64(x), Issuer: gw, Audience: lmbp.TokenAudience(gw, tenant), SaaSName: strings.ToUpper(tenant)}
}

// A v1 install (one device_id + device_key.enc) migrates to one binding;
// the pairing keeps working and the originals are backed up.
func TestMigrateV1KeepsPairing(t *testing.T) {
	dir := t.TempDir()
	v1 := localIdentity{DataKey: strings.Repeat("ab", 32), DeviceID: "dev_1", RemoteURL: "https://saas.example"}
	v := fakeIdentity("https://gw.example", "acme", "dev_1")
	if err := saveDevice(dir, v1, v); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v1)
	_ = os.WriteFile(filepath.Join(dir, "identity.json"), b, 0o600)
	origKey, _ := os.ReadFile(filepath.Join(dir, deviceKeyFile))

	id, err := loadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if id.Version != 2 || len(id.Bindings) != 1 || id.DeviceID != "" {
		t.Fatalf("not migrated: %+v", id)
	}
	bd := id.Bindings[0]
	if bd.DeviceID != "dev_1" || bd.Tenant != "acme" || bd.Issuer != "https://gw.example" || bd.RemoteURL != "https://saas.example" || !bd.Migrated || len(bd.Policy.Allow) != 1 || bd.Policy.Allow[0] != "*" {
		t.Fatalf("binding %+v", bd)
	}
	got, err := loadBinding(dir, id, bd)
	if err != nil || got.DeviceID != "dev_1" || got.E2EEKey != v.E2EEKey {
		t.Fatalf("migrated key: %v %v", got, err)
	}
	if bak, _ := os.ReadFile(filepath.Join(dir, deviceKeyFile+".v1.bak")); string(bak) != string(origKey) {
		t.Fatal("device key backup missing")
	}
	var old localIdentity
	if b, err := os.ReadFile(filepath.Join(dir, "identity.json.v1.bak")); err != nil || json.Unmarshal(b, &old) != nil || old.DeviceID != "dev_1" {
		t.Fatalf("identity backup missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, deviceKeyFile)); !os.IsNotExist(err) {
		t.Fatal("v1 key file should be replaced by its backup")
	}
	// idempotent
	again, err := loadIdentity(dir)
	if err != nil || len(again.Bindings) != 1 || again.Bindings[0].ID != bd.ID {
		t.Fatalf("second load %+v %v", again, err)
	}
	core, err := newCore(dir, again)
	if err != nil {
		t.Fatal(err)
	}
	runtime.local, runtime.core = again, core
	defer func() { runtime.core = nil }()
	st := statusLocked()
	cs := st["connections"].([]map[string]any)
	if st["paired"] != true || len(cs) != 1 || cs[0]["tenant"] != "acme" || st["tenant"] != "acme" || st["remote_url"] != "https://saas.example" {
		t.Fatalf("status %v", st)
	}
}

// A binding whose key file is unreadable is reported, not fatal, and the
// other bindings still load.
func TestBrokenBindingIsIsolated(t *testing.T) {
	dir := t.TempDir()
	id := localIdentity{Version: 2, DataKey: strings.Repeat("ab", 32)}
	good := binding{ID: "b_good", DeviceID: "dev_g", Issuer: "https://gw.a", Tenant: "a", KeyFile: bindingKeyFile("b_good")}
	bad := binding{ID: "b_bad", DeviceID: "dev_b", Issuer: "https://gw.b", Tenant: "b", KeyFile: bindingKeyFile("b_bad")}
	if err := saveBinding(dir, id, good, fakeIdentity("https://gw.a", "a", "dev_g")); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(dir, "bindings", "b_bad"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, bad.KeyFile), []byte("garbage"), 0o600)
	id.Bindings = []binding{good, bad}
	core, err := newCore(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	runtime.local, runtime.core, runtime.dir = id, core, dir
	defer func() { runtime.core = nil }()
	cs := statusLocked()["connections"].([]map[string]any)
	if len(cs) != 2 || cs[0]["state"] == "error" || cs[1]["state"] != "error" || cs[1]["last_error"] == "" {
		t.Fatalf("connections %v", cs)
	}
	// revoking only the broken one keeps the good one
	var r struct {
		OK    bool
		Error string
	}
	_ = json.Unmarshal([]byte(call(`{"action":"connection_revoke","connection_id":"b_bad"}`)), &r)
	if !r.OK {
		t.Fatal(r.Error)
	}
	if _, err := os.Stat(filepath.Join(dir, "bindings", "b_bad")); !os.IsNotExist(err) {
		t.Fatal("revoked binding files remain")
	}
	if _, err := os.Stat(filepath.Join(dir, good.KeyFile)); err != nil {
		t.Fatal("other binding's key was touched")
	}
	_ = json.Unmarshal([]byte(call(`{"action":"disconnect"}`)), &r) // exactly one left: allowed without id
	if !r.OK || len(runtime.local.Bindings) != 0 {
		t.Fatalf("legacy disconnect: %v", r.Error)
	}
}

func TestPolicyPersisted(t *testing.T) {
	dir := t.TempDir()
	id := localIdentity{Version: 2, DataKey: strings.Repeat("ab", 32)}
	b := binding{ID: "b_1", DeviceID: "dev_1", Issuer: "https://gw.a", Tenant: "a", KeyFile: bindingKeyFile("b_1"), Policy: policyAll()}
	_ = saveBinding(dir, id, b, fakeIdentity("https://gw.a", "a", "dev_1"))
	id.Bindings = []binding{b}
	core, _ := newCore(dir, id)
	runtime.local, runtime.core, runtime.dir = id, core, dir
	defer func() { runtime.core = nil }()
	out := call(`{"action":"connection_policy_set","connection_id":"b_1","policy":{"allow":["lm/m1"],"max_concurrency":2}}`)
	if !strings.Contains(out, `"ok":true`) {
		t.Fatal(out)
	}
	if bad := call(`{"action":"connection_policy_set","connection_id":"b_1","policy":{"allow":["nomodel"]}}`); !strings.Contains(bad, `"ok":false`) {
		t.Fatal("invalid allow entry accepted")
	}
	again, _ := loadIdentity(dir)
	if p := again.Bindings[0].Policy; len(p.Allow) != 1 || p.Allow[0] != "lm/m1" || p.MaxConcurrency != 2 {
		t.Fatalf("policy %+v", p)
	}
}

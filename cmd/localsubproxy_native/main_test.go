package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"localsubproxy/internal/lsp2"
	"localsubproxy/internal/lsp2/client"
)

func TestLegacyIdentityRemoteURLMigration(t *testing.T) {
	var identity localIdentity
	if err := json.Unmarshal([]byte(`{"journal_key":"key","saas_url":"https://remote.example","gateway_url":"https://gateway.example"}`), &identity); err != nil {
		t.Fatal(err)
	}
	if !identity.normalize() || identity.RemoteURL != "https://remote.example" {
		t.Fatalf("legacy connection URL not restored: %#v", identity)
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "saas_url") || !strings.Contains(string(encoded), `"remote_url":"https://remote.example"`) {
		t.Fatalf("identity migration was not persisted under the new name: %s", encoded)
	}
}

func TestV2KeyIsEncryptedAtRest(t *testing.T) {
	dir := t.TempDir()
	id := localIdentity{JournalKey: strings.Repeat("ab", 32), Protocol: "v2", V2DeviceID: "dev_1"}
	v := client.Identity{GatewayURL: "https://gw.example", Tenant: "acme", DeviceID: "dev_1", PrivateKey: lsp2.B64(make([]byte, 64)), GatewayKey: lsp2.B64(make([]byte, 32))}
	if err := saveV2(dir, id, v); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, v2KeyFile))
	if strings.Contains(string(raw), v.PrivateKey) || strings.Contains(string(raw), "gw.example") {
		t.Fatal("device key stored in plaintext")
	}
	got, err := loadV2(dir, id)
	if err != nil || got.DeviceID != "dev_1" {
		t.Fatalf("roundtrip: %v %v", got, err)
	}
	id.JournalKey = strings.Repeat("cd", 32)
	if _, err := loadV2(dir, id); err == nil {
		t.Fatal("decrypted with the wrong key")
	}
}

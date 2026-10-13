package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"localsubproxy/internal/lsp2"
	"localsubproxy/internal/lsp2/client"
)

func TestDeviceKeysAreEncryptedAtRest(t *testing.T) {
	dir := t.TempDir()
	id := localIdentity{DataKey: strings.Repeat("ab", 32), DeviceID: "dev_1"}
	x, _ := lsp2.NewX25519()
	v := client.Identity{GatewayURL: "https://gw.example", Tenant: "acme", DeviceID: "dev_1", PrivateKey: lsp2.B64(make([]byte, 64)), GatewayKey: lsp2.B64(make([]byte, 32)), E2EEKey: lsp2.B64(x)}
	if err := saveDevice(dir, id, v); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, deviceKeyFile))
	if strings.Contains(string(raw), v.PrivateKey) || strings.Contains(string(raw), v.E2EEKey) || strings.Contains(string(raw), "gw.example") {
		t.Fatal("device keys stored in plaintext")
	}
	got, err := loadDevice(dir, id)
	if err != nil || got.DeviceID != "dev_1" || got.E2EEKey != v.E2EEKey {
		t.Fatalf("roundtrip: %v %v", got, err)
	}
	id.DataKey = strings.Repeat("cd", 32)
	if _, err := loadDevice(dir, id); err == nil {
		t.Fatal("decrypted with the wrong key")
	}
}

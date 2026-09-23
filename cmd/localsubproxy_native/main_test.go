package main

import (
	"encoding/json"
	"strings"
	"testing"
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

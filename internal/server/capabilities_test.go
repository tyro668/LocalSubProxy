package server

import (
	"path/filepath"
	"testing"

	"localsubproxy/internal/protocol"
)

func TestOlderCapabilitySnapshotCannotReplaceNewer(t *testing.T) {
	s, err := New(Config{DataFile: filepath.Join(t.TempDir(), "server.json"), RemoteToken: "remote-token-0123456789-0123456789-abcd", DeviceID: "dev_1", DeviceToken: "device-token-0123456789-0123456789-abc"})
	if err != nil {
		t.Fatal(err)
	}
	newer := protocol.Heartbeat{SnapshotVersion: 2, Adapters: []protocol.AdapterHealth{{AdapterID: "first"}, {AdapterID: "second"}}}
	older := protocol.Heartbeat{SnapshotVersion: 1, Adapters: []protocol.AdapterHealth{{AdapterID: "first"}}}
	s.setAdapters(newer)
	s.setAdapters(older)
	if len(s.adapters) != 2 || s.adapters["second"].AdapterID != "second" {
		t.Fatalf("stale heartbeat removed adapter: %#v", s.adapters)
	}
}

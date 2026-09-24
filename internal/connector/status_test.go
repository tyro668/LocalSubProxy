package connector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"localsubproxy/internal/server"
)

func TestStatusDropsWhenGatewayStops(t *testing.T) {
	dir := t.TempDir()
	gateway, err := server.New(server.Config{
		DataFile:    filepath.Join(dir, "server.json"),
		RemoteToken: "remote-token-0123456789-0123456789-abcd",
		DeviceID:    "dev_1",
		DeviceToken: "device-token-0123456789-0123456789-abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	cloud := httptest.NewServer(gateway.Handler())
	defer cloud.Close()
	client, err := New(Config{
		ServerURL:   cloud.URL,
		DeviceID:    "dev_1",
		DeviceToken: "device-token-0123456789-0123456789-abc",
		JournalFile: filepath.Join(dir, "journal.enc"),
		JournalKey:  "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.Status()["connected"] != false {
		t.Fatal("connected before any gateway contact")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ended := make(chan struct{})
	go func() { defer close(ended); _ = client.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && client.Status()["connected"] != true {
		time.Sleep(10 * time.Millisecond)
	}
	if client.Status()["connected"] != true {
		t.Fatal("not connected after heartbeat")
	}
	cloud.CloseClientConnections()
	cloud.Close()
	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && client.Status()["connected"] == true {
		time.Sleep(20 * time.Millisecond)
	}
	if client.Status()["connected"] == true {
		t.Fatal("still connected after the gateway stopped")
	}
	cancel()
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("connector did not stop")
	}
}

func TestStatusFollowsRemoteServiceWhileGatewayStaysOnline(t *testing.T) {
	dir := t.TempDir()
	gateway, err := server.New(server.Config{
		DataFile:    filepath.Join(dir, "server.json"),
		RemoteToken: "remote-token-0123456789-0123456789-abcd",
		DeviceID:    "dev_1",
		DeviceToken: "device-token-0123456789-0123456789-abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	cloud := httptest.NewServer(gateway.Handler())
	defer cloud.Close()
	remote := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"apiBaseUrl":"http://mock.example/api"}}`))
	}))
	defer remote.Close()
	client, err := New(Config{
		ServerURL:   cloud.URL,
		RemoteURL:   "http://" + remote.Listener.Addr().String(),
		DeviceID:    "dev_1",
		DeviceToken: "device-token-0123456789-0123456789-abc",
		JournalFile: filepath.Join(dir, "journal.enc"),
		JournalKey:  "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ended := make(chan struct{})
	go func() { defer close(ended); _ = client.Run(ctx) }()
	if client.Status()["connected"] == true {
		t.Fatal("remote service has not started")
	}
	remote.Start()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && client.Status()["connected"] != true {
		time.Sleep(20 * time.Millisecond)
	}
	if client.Status()["connected"] != true {
		t.Fatal("client did not connect when the remote service started")
	}
	remote.CloseClientConnections()
	remote.Close()
	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && client.Status()["connected"] == true {
		time.Sleep(20 * time.Millisecond)
	}
	if client.Status()["connected"] == true {
		t.Fatal("client stayed connected after the remote service stopped")
	}
	cancel()
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("connector did not stop")
	}
}

func TestStatusRequiresAuthenticatedGatewayHeartbeat(t *testing.T) {
	var allowHeartbeat atomic.Bool
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v1/connector/heartbeat" {
			if !allowHeartbeat.Load() || r.Header.Get("Authorization") != "Bearer device-token-0123456789-0123456789-abc" {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		http.NotFound(w, r)
	}))
	dir := t.TempDir()
	client, err := New(Config{
		ServerURL:   cloud.URL,
		DeviceID:    "dev_1",
		DeviceToken: "device-token-0123456789-0123456789-abc",
		JournalFile: filepath.Join(dir, "journal.enc"),
		JournalKey:  "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
	})
	if err != nil {
		t.Fatal(err)
	}
	hb := client.probeAll(context.Background())
	if err := client.sendHeartbeat(context.Background(), hb); err == nil {
		t.Fatal("unauthorized heartbeat unexpectedly succeeded")
	}
	if client.Status()["connected"] == true {
		t.Fatal("public health endpoint must not imply a device connection")
	}
	allowHeartbeat.Store(true)
	if err := client.sendHeartbeat(context.Background(), hb); err != nil {
		t.Fatal(err)
	}
	if client.Status()["connected"] != true {
		t.Fatal("authenticated heartbeat did not mark the gateway connected")
	}
	client.markPoll(false)
	if err := client.sendHeartbeat(context.Background(), hb); err != nil {
		t.Fatal(err)
	}
	if client.Status()["connected"] == true {
		t.Fatal("heartbeat cannot mask a broken command poll")
	}
	client.markPoll(true)
	if client.Status()["connected"] != true {
		t.Fatal("successful command poll did not restore the connection")
	}
	cloud.Close()
	started := time.Now()
	if err := client.sendHeartbeat(context.Background(), hb); err == nil {
		t.Fatal("stopped gateway unexpectedly accepted a heartbeat")
	}
	if client.Status()["connected"] == true {
		t.Fatal("stopped gateway still reported connected")
	}
	if time.Since(started) > 3*time.Second {
		t.Fatalf("disconnect detection took %s", time.Since(started))
	}
}

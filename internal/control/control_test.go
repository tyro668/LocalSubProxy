package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func startServer(t *testing.T, dir string, h Handler) {
	t.Helper()
	l, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = Serve(ctx, l, h) }()
	t.Cleanup(func() { cancel(); <-done })
}

func decode(t *testing.T, raw json.RawMessage) (bool, string) {
	t.Helper()
	var r struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("reply %q: %v", raw, err)
	}
	return r.OK, r.Error
}

func TestRoundTripAndRefusals(t *testing.T) {
	dir := shortDir(t)
	var got []string
	startServer(t, dir, func(req string) string {
		got = append(got, req)
		return `{"ok":true,"data":{"echo":1}}`
	})
	c, err := Connect(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < 2; i++ { // several requests on one connection
		r, err := c.Do(map[string]any{"action": "status"})
		if err != nil {
			t.Fatal(err)
		}
		if ok, _ := decode(t, r); !ok {
			t.Fatalf("status: %s", r)
		}
	}
	r, _ := c.Do(map[string]any{"action": "stop"})
	if ok, msg := decode(t, r); ok || !strings.Contains(msg, "not supported") {
		t.Fatalf("stop must be refused explicitly, got %s", r)
	}
	r, _ = c.Do(map[string]any{"id": "x"})
	if ok, msg := decode(t, r); ok || msg != "action required" {
		t.Fatalf("missing action: %s", r)
	}
	if len(got) != 2 {
		t.Fatalf("handler saw %d requests, want 2 (stop/invalid must not reach it)", len(got))
	}
}

func TestConnectWithoutServe(t *testing.T) {
	_, err := Connect(shortDir(t))
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("want a not-running error, got %v", err)
	}
}

func TestSecondListenerRefused(t *testing.T) {
	dir := shortDir(t)
	startServer(t, dir, func(string) string { return `{"ok":true}` })
	time.Sleep(50 * time.Millisecond)
	if l, err := Listen(dir); err == nil {
		_ = l.Close()
		t.Fatal("a second serve could take over the control channel")
	}
}

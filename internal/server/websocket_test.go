package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"localsubproxy/internal/protocol"
)

func TestWebSocketEpochAndDurableEventACK(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	cfg := Config{DataFile: path, RemoteToken: "remote-token-0123456789-0123456789-abcd", DeviceID: "dev_1", DeviceToken: "device-token-0123456789-0123456789-abc"}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cloud := httptest.NewServer(s.Handler())
	defer cloud.Close()
	dial := func(base string) (*websocket.Conn, protocol.Frame) {
		t.Helper()
		header := http.Header{}
		header.Set("Authorization", "Bearer "+cfg.DeviceToken)
		conn, resp, err := (&websocket.Dialer{Subprotocols: []string{protocol.Version}}).Dial("ws"+strings.TrimPrefix(base, "http")+"/v1/connector/connect", header)
		if err != nil {
			if resp != nil {
				t.Fatalf("dial: %v status %s", err, resp.Status)
			}
			t.Fatal(err)
		}
		hb := protocol.Heartbeat{Protocol: protocol.Version, DeviceID: cfg.DeviceID, Adapter: protocol.AdapterHealth{AdapterID: "ollama", AdapterType: "ollama_http", Health: "ready", Models: []string{"model"}, Operations: []string{"chat.generate"}, FreeSlots: 1}}
		b, _ := json.Marshal(protocol.Hello{Heartbeat: hb})
		if err := conn.WriteJSON(protocol.Frame{Protocol: protocol.Version, Type: "hello", ID: "hello", Payload: b}); err != nil {
			t.Fatal(err)
		}
		var welcome protocol.Frame
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if err := conn.ReadJSON(&welcome); err != nil {
			t.Fatal(err)
		}
		if welcome.Type != "welcome" || welcome.SessionEpoch <= 0 {
			t.Fatalf("welcome: %#v", welcome)
		}
		return conn, welcome
	}
	first, w1 := dial(cloud.URL)
	defer first.Close()
	second, w2 := dial(cloud.URL)
	defer second.Close()
	if w2.SessionEpoch <= w1.SessionEpoch {
		t.Fatalf("epochs %d %d", w1.SessionEpoch, w2.SessionEpoch)
	}
	_ = first.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := first.ReadMessage(); err == nil {
		t.Fatal("old session was not closed")
	}
	// A frame with an old epoch on the new socket cannot change server state.
	bad := protocol.Frame{Protocol: protocol.Version, Type: "heartbeat", ID: "bad", SessionEpoch: w1.SessionEpoch, Payload: json.RawMessage(`{}`)}
	if err := second.WriteJSON(bad); err != nil {
		t.Fatal(err)
	}
	_ = second.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := second.ReadMessage(); err == nil {
		t.Fatal("stale epoch accepted")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.wsMu.Lock()
		active := s.active
		s.wsMu.Unlock()
		if active == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.wsMu.Lock()
	active := s.active
	s.wsMu.Unlock()
	if active != nil {
		t.Fatal("closed WebSocket blocked HTTPS fallback")
	}
	// The next server process must continue from the persisted epoch.
	s2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cloud2 := httptest.NewServer(s2.Handler())
	defer cloud2.Close()
	third, w3 := dial(cloud2.URL)
	defer third.Close()
	if w3.SessionEpoch <= w2.SessionEpoch {
		t.Fatalf("epoch did not survive restart: %d <= %d", w3.SessionEpoch, w2.SessionEpoch)
	}
	// Event ACK only follows a durable write, including an exact replay.
	s2.mu.Lock()
	ev := protocol.NewEvent(1, "task.failed", map[string]string{"code": "TEST"})
	s2.state.Tasks["task_1"] = &record{Task: protocol.Task{ID: "task_1", AttemptID: "attempt_1", FenceToken: 1, State: "ACCEPTED"}}
	if err := s2.persist(); err != nil {
		s2.mu.Unlock()
		t.Fatal(err)
	}
	s2.mu.Unlock()
	batch := protocol.EventBatch{TaskID: "task_1", AttemptID: "attempt_1", FenceToken: 1, Events: []protocol.Event{ev}}
	b, _ := json.Marshal(batch)
	current, epoch := third, w3.SessionEpoch
	for i, id := range []string{"event1", "event2"} {
		if i == 1 {
			third.Close() // ACK was received, but replay still has to be idempotent.
			fourth, w4 := dial(cloud2.URL)
			defer fourth.Close()
			if w4.SessionEpoch <= epoch {
				t.Fatal("reconnect did not advance epoch")
			}
			current, epoch = fourth, w4.SessionEpoch
		}
		if err := current.WriteJSON(protocol.Frame{Protocol: protocol.Version, Type: "task.events", ID: id, SessionEpoch: epoch, Payload: b}); err != nil {
			t.Fatal(err)
		}
		var ack protocol.Frame
		_ = current.SetReadDeadline(time.Now().Add(2 * time.Second))
		if err := current.ReadJSON(&ack); err != nil {
			t.Fatal(err)
		}
		if ack.ReplyTo != id || ack.Error != "" {
			t.Fatalf("event ack: %#v", ack)
		}
		var payload struct {
			AckSeq int64 `json:"ack_seq"`
		}
		if err := json.Unmarshal(ack.Payload, &payload); err != nil || payload.AckSeq != 1 {
			t.Fatalf("ack payload: %#v %v", payload, err)
		}
	}
	reopened, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.state.Tasks["task_1"].Task.AckSeq != 1 || len(reopened.state.Tasks["task_1"].Task.Events) != 1 {
		t.Fatal("ACK preceded persistence or replay duplicated event")
	}
}

func TestWebSocketHandshakeAuthentication(t *testing.T) {
	s, err := New(Config{DataFile: filepath.Join(t.TempDir(), "server.json"), RemoteToken: "remote-token-0123456789-0123456789-abcd", DeviceID: "dev_1", DeviceToken: "device-token-0123456789-0123456789-abc"})
	if err != nil {
		t.Fatal(err)
	}
	cloud := httptest.NewServer(s.Handler())
	defer cloud.Close()
	endpoint := "ws" + strings.TrimPrefix(cloud.URL, "http") + "/v1/connector/connect"
	for _, tc := range []struct {
		auth, origin string
		want         int
	}{
		{"device-token-0123456789-0123456789-abc", "", 401},
		{"Bearer wrong", "", 401},
		{"Bearer device-token-0123456789-0123456789-abc", "https://other.example", 403},
	} {
		header := http.Header{}
		header.Set("Authorization", tc.auth)
		if tc.origin != "" {
			header.Set("Origin", tc.origin)
		}
		conn, resp, err := (&websocket.Dialer{Subprotocols: []string{protocol.Version}}).Dial(endpoint, header)
		if conn != nil {
			conn.Close()
			t.Fatal("unauthorized handshake succeeded")
		}
		if err == nil || resp == nil || resp.StatusCode != tc.want {
			t.Fatalf("handshake auth=%q origin=%q: %v %#v", tc.auth, tc.origin, err, resp)
		}
		resp.Body.Close()
	}
}

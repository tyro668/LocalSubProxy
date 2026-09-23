package server

import (
	"bytes"
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

func devicePost(s *Server, path string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer "+s.cfg.DeviceToken)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestLeaseExpirationAndReconciliation(t *testing.T) {
	s, err := New(Config{DataFile: filepath.Join(t.TempDir(), "server.json"), RemoteToken: "remote-token-0123456789-0123456789-abcd", DeviceID: "dev_1", DeviceToken: "device-token-0123456789-0123456789-abc"})
	if err != nil {
		t.Fatal(err)
	}
	task := protocol.Task{ID: "task_1", AttemptID: "attempt_1", FenceToken: 1, State: "ACCEPTED", ExpiresAt: time.Now().Add(time.Minute)}
	task.Request.Options.MaxRuntimeSeconds = 300
	s.state.Tasks[task.ID] = &record{Task: task}
	if err := s.persist(); err != nil {
		t.Fatal(err)
	}
	ref := map[string]any{"task_id": task.ID, "attempt_id": task.AttemptID, "fence_token": int64(1)}
	grant := devicePost(s, "/v1/connector/start", ref)
	if grant.Code != 200 {
		t.Fatalf("grant: %d %s", grant.Code, grant.Body.String())
	}
	var response struct {
		LeaseUntil time.Time `json:"lease_until"`
	}
	if err := json.Unmarshal(grant.Body.Bytes(), &response); err != nil || response.LeaseUntil.Before(time.Now().Add(time.Minute)) {
		t.Fatalf("lease grant: %+v %v", response, err)
	}
	started := protocol.NewEvent(1, "task.started", map[string]string{"run_id": "r1"})
	ack := devicePost(s, "/v1/connector/events", protocol.EventBatch{TaskID: task.ID, AttemptID: task.AttemptID, FenceToken: 1, Events: []protocol.Event{started}})
	if ack.Code != 200 {
		t.Fatalf("start event: %d %s", ack.Code, ack.Body.String())
	}
	s.mu.Lock()
	s.state.Tasks[task.ID].Task.LeaseUntil = time.Now().Add(-time.Second)
	if err := s.persist(); err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	s.mu.Unlock()
	if got := devicePost(s, "/v1/connector/lease", ref); got.Code != 409 {
		t.Fatalf("expired lease renewed: %d", got.Code)
	}
	if s.state.Tasks[task.ID].Task.State != "RECONCILING" {
		t.Fatalf("state %s", s.state.Tasks[task.ID].Task.State)
	}
	unknown := protocol.NewEvent(2, "task.unknown", map[string]string{"code": "LEASE_EXPIRED"})
	ack = devicePost(s, "/v1/connector/events", protocol.EventBatch{TaskID: task.ID, AttemptID: task.AttemptID, FenceToken: 1, Events: []protocol.Event{unknown}})
	if ack.Code != 200 || s.state.Tasks[task.ID].Task.State != "UNKNOWN" {
		t.Fatalf("reconcile result %d state %s", ack.Code, s.state.Tasks[task.ID].Task.State)
	}
}

func TestDeviceRevokePersistsAndRejectsReconnect(t *testing.T) {
	cfg := Config{DataFile: filepath.Join(t.TempDir(), "server.json"), RemoteToken: "remote-token-0123456789-0123456789-abcd", DeviceID: "dev_1", DeviceToken: "device-token-0123456789-0123456789-abc"}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.state.Tasks["task_1"] = &record{Task: protocol.Task{ID: "task_1", State: "RUNNING"}}
	if err := s.persist(); err != nil {
		t.Fatal(err)
	}
	cloud := httptest.NewServer(s.Handler())
	defer cloud.Close()
	header := http.Header{}
	header.Set("Authorization", "Bearer "+cfg.DeviceToken)
	endpoint := "ws" + strings.TrimPrefix(cloud.URL, "http") + "/v1/connector/connect"
	conn, _, err := (&websocket.Dialer{Subprotocols: []string{protocol.Version}}).Dial(endpoint, header)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	hello, _ := json.Marshal(protocol.Hello{Heartbeat: protocol.Heartbeat{Protocol: protocol.Version, DeviceID: cfg.DeviceID}})
	if err := conn.WriteJSON(protocol.Frame{Protocol: protocol.Version, Type: "hello", ID: "hello", Payload: hello}); err != nil {
		t.Fatal(err)
	}
	var welcome protocol.Frame
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if err := conn.ReadJSON(&welcome); err != nil {
		t.Fatal(err)
	}
	if welcome.Type != "welcome" {
		t.Fatal("WSS enrollment failed")
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/connectors/dev_1/revoke", nil)
	r.Header.Set("Authorization", "Bearer "+cfg.RemoteToken)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("revoke %d %s", w.Code, w.Body.String())
	}
	if s.state.Tasks["task_1"].Task.State != "CANCEL_REQUESTED" {
		t.Fatal("running task was not marked for cancellation")
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("revoked WSS remained active")
	}
	if later, response, err := (&websocket.Dialer{Subprotocols: []string{protocol.Version}}).Dial(endpoint, header); err == nil || response == nil || response.StatusCode != 403 {
		if later != nil {
			later.Close()
		}
		t.Fatalf("revoked reconnect: %v %#v", err, response)
	}
	if got := devicePost(s, "/v1/connector/heartbeat", protocol.Heartbeat{Protocol: protocol.Version, DeviceID: cfg.DeviceID}); got.Code != 403 {
		t.Fatalf("revoked device status %d", got.Code)
	}
	reopened, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.state.Revoked {
		t.Fatal("revocation was not durable")
	}
}

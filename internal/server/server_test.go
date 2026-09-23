package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"localsubproxy/internal/protocol"
)

func TestEventReplayIntegrityAndBatchRollback(t *testing.T) {
	s, err := New(Config{DataFile: filepath.Join(t.TempDir(), "server.json"), SAASToken: "saas-token-0123456789-0123456789-abcd", DeviceID: "dev_1", DeviceToken: "device-token-0123456789-0123456789-abc"})
	if err != nil {
		t.Fatal(err)
	}
	first := protocol.NewEvent(1, "task.started", map[string]string{"run_id": "r1"})
	s.state.Tasks["task_1"] = &record{Task: protocol.Task{ID: "task_1", AttemptID: "attempt_1", FenceToken: 1, State: "RUNNING", AckSeq: 1, Events: []protocol.Event{first}, CreatedAt: time.Now()}}
	if err := s.persist(); err != nil {
		t.Fatal(err)
	}
	send := func(events ...protocol.Event) int {
		t.Helper()
		b, _ := json.Marshal(protocol.EventBatch{TaskID: "task_1", AttemptID: "attempt_1", FenceToken: 1, Events: events})
		r := httptest.NewRequest("POST", "/v1/connector/events", bytes.NewReader(b))
		r.Header.Set("Authorization", "Bearer "+s.cfg.DeviceToken)
		w := httptest.NewRecorder()
		s.events(w, r)
		return w.Code
	}
	if got := send(first); got != 200 {
		t.Fatalf("exact replay status %d", got)
	}
	changed := first
	changed.Data = json.RawMessage(`{"run_id":"different"}`)
	if got := send(changed); got != 409 {
		t.Fatalf("conflicting replay status %d", got)
	}
	second := protocol.NewEvent(2, "output.delta", map[string]string{"text": "ok"})
	invalid := protocol.NewEvent(4, "output.delta", map[string]string{"text": "gap"})
	if got := send(second, invalid); got != 409 {
		t.Fatalf("gap status %d", got)
	}
	if s.state.Tasks["task_1"].Task.AckSeq != 1 {
		t.Fatal("invalid batch partially committed")
	}
	if got := send(second); got != 200 {
		t.Fatalf("valid event status %d", got)
	}
	if s.state.Tasks["task_1"].Task.AckSeq != 2 {
		t.Fatal("valid event not committed")
	}
}

func TestQueuedTaskExpiresWithoutConnectedDevice(t *testing.T) {
	s, err := New(Config{DataFile: filepath.Join(t.TempDir(), "server.json"), SAASToken: "saas-token-0123456789-0123456789-abcd", DeviceID: "dev_1", DeviceToken: "device-token-0123456789-0123456789-abc"})
	if err != nil {
		t.Fatal(err)
	}
	s.state.Tasks["task_1"] = &record{Task: protocol.Task{ID: "task_1", State: "QUEUED", ExpiresAt: time.Now().Add(-time.Second)}}
	if err := s.persist(); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.getTask(w, "task_1")
	if w.Code != 200 || s.state.Tasks["task_1"].Task.State != "EXPIRED" {
		t.Fatalf("queued task: status=%d state=%s", w.Code, s.state.Tasks["task_1"].Task.State)
	}
}

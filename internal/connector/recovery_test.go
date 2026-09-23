package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"localsubproxy/internal/durable"
	"localsubproxy/internal/protocol"
)

func TestRestartAfterStartIntentIsUnknown(t *testing.T) {
	const key = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	path := filepath.Join(t.TempDir(), "journal.enc")
	cipher, err := durable.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	cmd := protocol.Command{TaskID: "task_1", AttemptID: "attempt_1", FenceToken: 1, Digest: "digest"}
	j := journal{Tasks: map[string]*localTask{"task_1": {Command: cmd, State: "RUNNING", StartIntent: true, Events: []protocol.Event{protocol.NewEvent(1, "task.started", map[string]string{"run_id": "attempt_1"})}}}}
	b, err := cipher.Seal(j)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{ServerURL: "http://127.0.0.1:8080", DeviceID: "dev_1", DeviceToken: "device-token-0123456789-0123456789-abc", AdapterID: "ollama-local", OllamaURL: "http://127.0.0.1:11434", AllowedModels: []string{"test-model"}, JournalFile: path, JournalKey: key})
	if err != nil {
		t.Fatal(err)
	}
	got := c.j.Tasks["task_1"]
	if got.State != "UNKNOWN" || len(got.Events) != 2 || got.Events[1].Kind != "task.unknown" {
		t.Fatalf("restart journal: %#v", got)
	}
	// The UNKNOWN result itself is durable across another restart.
	second, err := New(c.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.j.Tasks["task_1"].Events) != 2 {
		t.Fatal("duplicate recovery event")
	}
}

func TestAcceptedJournalResumesWithoutDuplicateDispatch(t *testing.T) {
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/chat" {
			calls.Add(1)
			fmt.Fprintln(w, `{"done":true}`)
			return
		}
		fmt.Fprint(w, `{"models":[{"name":"test-model"}]}`)
	}))
	defer model.Close()
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/connector/accepted":
			fmt.Fprint(w, `{"state":"ACCEPTED"}`)
		case "/v1/connector/start":
			fmt.Fprintf(w, `{"grant":"g1","expires_at":%q}`, time.Now().Add(time.Minute).Format(time.RFC3339Nano))
		case "/v1/connector/events":
			var batch protocol.EventBatch
			json.NewDecoder(r.Body).Decode(&batch)
			fmt.Fprintf(w, `{"ack_seq":%d}`, batch.Events[len(batch.Events)-1].Seq)
		default:
			http.NotFound(w, r)
		}
	}))
	defer cloud.Close()
	const key = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	path := filepath.Join(t.TempDir(), "journal.enc")
	req := protocol.CreateTask{Target: protocol.Target{DeviceID: "dev_1", AdapterID: "ollama-local", ModelID: "test-model"}, Operation: "chat.generate", Input: protocol.Input{Messages: []protocol.Message{{Role: "user", Content: "hi"}}}}
	req.Options.MaxRuntimeSeconds = 10
	cmd := protocol.Command{TaskID: "task_1", AttemptID: "attempt_1", FenceToken: 1, Request: req, Digest: protocol.Digest(req)}
	cipher, _ := durable.NewCipher(key)
	data, _ := cipher.Seal(journal{Tasks: map[string]*localTask{"task_1": {Command: cmd, State: "ACCEPTED"}}})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{ServerURL: cloud.URL, DeviceID: "dev_1", DeviceToken: "device-token-0123456789-0123456789-abc", AdapterID: "ollama-local", OllamaURL: model.URL, AllowedModels: []string{"test-model"}, JournalFile: path, JournalKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.resumeAccepted(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		state := c.j.Tasks["task_1"].State
		c.mu.Unlock()
		if state == "SUCCEEDED" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.mu.Lock()
	state := c.j.Tasks["task_1"].State
	c.mu.Unlock()
	if state != "SUCCEEDED" || calls.Load() != 1 {
		t.Fatalf("recovered state=%s calls=%d", state, calls.Load())
	}
}

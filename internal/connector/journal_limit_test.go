package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"localsubproxy/internal/protocol"
)

func TestJournalLimitReserveAndAckCleanup(t *testing.T) {
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch protocol.EventBatch
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Error(err)
			return
		}
		fmt.Fprintf(w, `{"ack_seq":%d}`, batch.Events[len(batch.Events)-1].Seq)
	}))
	defer cloud.Close()
	cfg := Config{ServerURL: cloud.URL, DeviceID: "dev_1", DeviceToken: "device-token-0123456789-0123456789-abc", AdapterID: "ollama-local", OllamaURL: "http://127.0.0.1:11434", AllowedModels: []string{"test-model"}, JournalFile: filepath.Join(t.TempDir(), "journal.enc"), JournalKey: "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f", MaxJournalBytes: 256 << 10}
	x, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cmd := protocol.Command{TaskID: "task_1", AttemptID: "attempt_1", FenceToken: 1, Request: protocol.CreateTask{Input: protocol.Input{Messages: []protocol.Message{{Role: "user", Content: "sensitive prompt"}}}}}
	x.mu.Lock()
	task := &localTask{Command: cmd, State: "RUNNING"}
	x.j.Tasks[cmd.TaskID] = task
	if err := x.saveLocked(); err != nil {
		x.mu.Unlock()
		t.Fatal(err)
	}
	var filled bool
	for i := 0; i < 8; i++ {
		err = x.appendSavedLocked(task, "output.delta", map[string]string{"text": strings.Repeat("x", 32<<10)})
		if errors.Is(err, errJournalFull) {
			filled = true
			break
		}
		if err != nil {
			x.mu.Unlock()
			t.Fatal(err)
		}
	}
	if !filled {
		x.mu.Unlock()
		t.Fatal("journal limit was not enforced")
	}
	before := task.NextSeq
	if err := x.appendSavedLocked(task, "task.failed", map[string]string{"code": "LOCAL_SPOOL_FULL"}); err != nil {
		x.mu.Unlock()
		t.Fatalf("terminal event had no reserved capacity: %v", err)
	}
	task.State = "FAILED"
	task.DoneAt = time.Now()
	x.mu.Unlock()
	if err := x.sendPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	x.mu.Lock()
	if task.AckSeq != before+1 || task.NextSeq != before+1 || len(task.Events) != 0 || len(task.Command.Request.Input.Messages) != 0 {
		x.mu.Unlock()
		t.Fatalf("ACK cleanup failed: %+v", task)
	}
	x.mu.Unlock()
	reopened, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.j.Tasks[cmd.TaskID]
	if got.NextSeq != before+1 || got.AckSeq != before+1 || len(got.Events) != 0 || len(got.Command.Request.Input.Messages) != 0 {
		t.Fatal("ACK cleanup was not durable")
	}
}

func TestExpiredLeaseCancelsLocalExecution(t *testing.T) {
	cfg := Config{ServerURL: "http://127.0.0.1:8080", DeviceID: "dev_1", DeviceToken: "device-token-0123456789-0123456789-abc", AdapterID: "ollama-local", OllamaURL: "http://127.0.0.1:11434", AllowedModels: []string{"test-model"}, JournalFile: filepath.Join(t.TempDir(), "journal.enc"), JournalKey: "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"}
	x, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	x.running["task_1"] = cancel
	x.j.Tasks["task_1"] = &localTask{Command: protocol.Command{TaskID: "task_1", AttemptID: "attempt_1", FenceToken: 1}, State: "RUNNING", StartIntent: true, LeaseUntil: time.Now().Add(-time.Second)}
	x.renewLeases(context.Background())
	select {
	case <-runCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("expired lease did not stop run")
	}
	if !x.leaseStopped["task_1"] {
		t.Fatal("lease expiry reason was not recorded")
	}
}

package connector_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"localsubproxy/internal/connector"
	"localsubproxy/internal/protocol"
	"localsubproxy/internal/server"
)

const deviceToken = "device-token-0123456789-0123456789-abc"
const remoteToken = "remote-token-0123456789-0123456789-abcd"
const journalKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func api(t *testing.T, client *http.Client, base, method, path, token string, body any, out any) int {
	t.Helper()
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	req, err := http.NewRequest(method, base+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

func TestEndToEndAndIdempotency(t *testing.T) {
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			fmt.Fprint(w, `{"models":[{"name":"test-model"}]}`)
		case "/api/chat":
			calls.Add(1)
			w.Header().Set("Content-Type", "application/x-ndjson")
			fmt.Fprintln(w, `{"message":{"content":"hello "},"done":false}`)
			fmt.Fprintln(w, `{"message":{"content":"world"},"done":false}`)
			fmt.Fprintln(w, `{"done":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer model.Close()
	dir := t.TempDir()
	s, err := server.New(server.Config{DataFile: filepath.Join(dir, "server.json"), RemoteToken: remoteToken, DeviceID: "dev_1", DeviceToken: deviceToken})
	if err != nil {
		t.Fatal(err)
	}
	cloud := httptest.NewServer(s.Handler())
	defer cloud.Close()
	c, err := connector.New(connector.Config{ServerURL: cloud.URL, DeviceID: "dev_1", DeviceToken: deviceToken, AdapterID: "ollama-local", OllamaURL: model.URL, AllowedModels: []string{"test-model"}, JournalFile: filepath.Join(dir, "journal.enc"), JournalKey: journalKey})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	defer func() { cancel(); <-runDone }()
	go func() { defer close(runDone); _ = c.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var v struct {
			Devices []struct {
				Status string `json:"status"`
			} `json:"devices"`
		}
		api(t, cloud.Client(), cloud.URL, "GET", "/v1/connectors", remoteToken, nil, &v)
		if len(v.Devices) > 0 && v.Devices[0].Status == "online" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	req := protocol.CreateTask{Target: protocol.Target{DeviceID: "dev_1", AdapterID: "ollama-local", ModelID: "test-model"}, Operation: "chat.generate", Input: protocol.Input{Messages: []protocol.Message{{Role: "user", Content: "hi"}}}}
	create := func() protocol.Task {
		body, _ := json.Marshal(req)
		r, _ := http.NewRequest("POST", cloud.URL+"/v1/llm-tasks", bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+remoteToken)
		r.Header.Set("Idempotency-Key", "one-key-123")
		resp, err := cloud.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 202 {
			t.Fatalf("create status %d", resp.StatusCode)
		}
		var task protocol.Task
		if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
			t.Fatal(err)
		}
		return task
	}
	first := create()
	second := create()
	if first.ID != second.ID {
		t.Fatal("idempotency key created two tasks")
	}
	var got protocol.Task
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		api(t, cloud.Client(), cloud.URL, "GET", "/v1/llm-tasks/"+first.ID, remoteToken, nil, &got)
		if got.State == "SUCCEEDED" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.State != "SUCCEEDED" {
		t.Fatalf("task state %s", got.State)
	}
	if calls.Load() != 1 {
		t.Fatalf("model called %d times", calls.Load())
	}
	if got.AckSeq != 4 {
		t.Fatalf("expected 4 events, got %d", got.AckSeq)
	}
	if got.Events[1].Kind != "output.delta" || got.Events[2].Kind != "output.delta" {
		t.Fatalf("unexpected stream %#v", got.Events)
	}
	// Restart reads the encrypted journal and must not make another model call.
	cancel()
	<-runDone
	_, err = connector.New(connector.Config{ServerURL: cloud.URL, DeviceID: "dev_1", DeviceToken: deviceToken, AdapterID: "ollama-local", OllamaURL: model.URL, AllowedModels: []string{"test-model"}, JournalFile: filepath.Join(dir, "journal.enc"), JournalKey: journalKey})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("model restarted")
	}
}

func TestCancelRunningTask(t *testing.T) {
	started := make(chan struct{}, 1)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			fmt.Fprint(w, `{"models":[{"name":"test-model"}]}`)
			return
		}
		started <- struct{}{}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer model.Close()
	dir := t.TempDir()
	s, err := server.New(server.Config{DataFile: filepath.Join(dir, "server.json"), RemoteToken: remoteToken, DeviceID: "dev_1", DeviceToken: deviceToken})
	if err != nil {
		t.Fatal(err)
	}
	cloud := httptest.NewServer(s.Handler())
	defer cloud.Close()
	c, err := connector.New(connector.Config{ServerURL: cloud.URL, DeviceID: "dev_1", DeviceToken: deviceToken, AdapterID: "ollama-local", OllamaURL: model.URL, AllowedModels: []string{"test-model"}, JournalFile: filepath.Join(dir, "journal.enc"), JournalKey: journalKey})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	defer func() { cancel(); <-runDone }()
	go func() { defer close(runDone); _ = c.Run(ctx) }()
	req := protocol.CreateTask{Target: protocol.Target{DeviceID: "dev_1", AdapterID: "ollama-local", ModelID: "test-model"}, Operation: "chat.generate", Input: protocol.Input{Messages: []protocol.Message{{Role: "user", Content: "wait"}}}}
	// Wait for the first heartbeat before submitting.
	time.Sleep(100 * time.Millisecond)
	body, _ := json.Marshal(req)
	r, _ := http.NewRequest("POST", cloud.URL+"/v1/llm-tasks", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+remoteToken)
	r.Header.Set("Idempotency-Key", "cancel-key-123")
	resp, err := cloud.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	var task protocol.Task
	json.NewDecoder(resp.Body).Decode(&task)
	resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("model never started")
	}
	if code := api(t, cloud.Client(), cloud.URL, "POST", "/v1/llm-tasks/"+task.ID+"/cancel", remoteToken, map[string]any{}, nil); code != 202 {
		t.Fatalf("cancel: %d", code)
	}
	deadline := time.Now().Add(5 * time.Second)
	var got protocol.Task
	for time.Now().Before(deadline) {
		api(t, cloud.Client(), cloud.URL, "GET", "/v1/llm-tasks/"+task.ID, remoteToken, nil, &got)
		if got.State == "CANCELLED" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.State != "CANCELLED" {
		t.Fatalf("state after cancel: %s", got.State)
	}
}

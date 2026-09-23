package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"localsubproxy/internal/protocol"
)

func TestTextArtifactScopedToTaskAttempt(t *testing.T) {
	cfg := Config{DataFile: filepath.Join(t.TempDir(), "server.json"), RemoteToken: "remote-token-0123456789-0123456789-abcd", DeviceID: "dev_1", DeviceToken: "device-token-0123456789-0123456789-abc"}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	upload := func(contentType string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/task-artifacts", bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+cfg.RemoteToken)
		r.Header.Set("Content-Type", contentType)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	if got := upload("application/octet-stream", []byte("text")); got.Code != 415 {
		t.Fatalf("binary MIME: %d", got.Code)
	}
	if got := upload("text/plain", bytes.Repeat([]byte("a"), (1<<20)+1)); got.Code != 413 {
		t.Fatalf("oversize: %d", got.Code)
	}
	if got := upload("text/plain", []byte{'a', 0, 'b'}); got.Code != 413 {
		t.Fatalf("NUL text: %d", got.Code)
	}
	created := upload("text/plain; charset=utf-8", []byte("Revenue rose 12%."))
	if created.Code != 201 {
		t.Fatalf("upload: %d %s", created.Code, created.Body.String())
	}
	var artifact struct {
		ID     string `json:"artifact_id"`
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &artifact); err != nil {
		t.Fatal(err)
	}
	if !validArtifactID(artifact.ID) {
		t.Fatal("invalid artifact ID")
	}
	request := protocol.CreateTask{Target: protocol.Target{DeviceID: cfg.DeviceID, AdapterID: "ollama-local", ModelID: "model"}, Operation: "chat.generate", Input: protocol.Input{Messages: []protocol.Message{{Role: "user", Content: "Summarize"}}, ArtifactRefs: []string{artifact.ID}}}
	request.Input.ArtifactRefs = []string{artifact.ID, artifact.ID}
	if err := request.Validate(); err == nil {
		t.Fatal("duplicate artifact reference accepted")
	}
	request.Input.ArtifactRefs = []string{artifact.ID}
	b, _ := json.Marshal(request)
	r := httptest.NewRequest(http.MethodPost, "/v1/llm-tasks", bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer "+cfg.RemoteToken)
	r.Header.Set("Idempotency-Key", "artifact-test-001")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 202 {
		t.Fatalf("task: %d %s", w.Code, w.Body.String())
	}
	var task protocol.Task
	if err := json.Unmarshal(w.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.state.Tasks[task.ID].Task.AttemptID = "attempt_1"
	s.state.Tasks[task.ID].Task.State = "ACCEPTED"
	err = s.persist()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	download := func(token, taskID, attemptID string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/v1/connector/artifacts/"+artifact.ID+"?task_id="+taskID+"&attempt_id="+attemptID, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	if got := download(cfg.RemoteToken, task.ID, "attempt_1"); got.Code != 401 {
		t.Fatalf("service token download: %d", got.Code)
	}
	if got := download(cfg.DeviceToken, task.ID, "attempt_wrong"); got.Code != 404 {
		t.Fatalf("wrong attempt: %d", got.Code)
	}
	if got := download(cfg.DeviceToken, "task_other", "attempt_1"); got.Code != 404 {
		t.Fatalf("wrong task: %d", got.Code)
	}
	got := download(cfg.DeviceToken, task.ID, "attempt_1")
	if got.Code != 200 || got.Body.String() != "Revenue rose 12%." || got.Header().Get("X-Content-SHA256") != artifact.SHA256 {
		t.Fatalf("download: %d %s", got.Code, got.Body.String())
	}
	if err := os.WriteFile(s.artifactPath(artifact.ID), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := download(cfg.DeviceToken, task.ID, "attempt_1"); got.Code != 503 {
		t.Fatalf("tampered artifact: %d", got.Code)
	}
	reopened, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.state.Artifacts[artifact.ID].TaskID != task.ID || !strings.HasPrefix(reopened.state.Artifacts[artifact.ID].SHA256, artifact.SHA256[:8]) {
		t.Fatal("artifact binding not durable")
	}
}

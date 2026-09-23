package connector_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"localsubproxy/internal/connector"
	"localsubproxy/internal/server"
)

func TestManagedOpenAIAdapterEndToEnd(t *testing.T) {
	const providerKey = "provider-secret-never-leave-device"
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1") && r.Header.Get("Authorization") != "Bearer "+providerKey {
			http.Error(w, "bad provider key", 401)
			return
		}
		switch r.URL.Path {
		case "/api/tags":
			_, _ = io.WriteString(w, `{"models":[{"name":"ollama-test"}]}`)
		case "/api/chat":
			_, _ = io.WriteString(w, "{\"message\":{\"content\":\"local\"},\"done\":false}\n{\"done\":true}\n")
		case "/api/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"test-model"}]}`)
		case "/api/v1/chat/completions":
			calls.Add(1)
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload["max_tokens"] != float64(1024) || payload["reasoning_effort"] != "medium" {
				t.Errorf("provider request did not include configured parameters: %#v, %v", payload, err)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"!\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	dir := t.TempDir()
	gateway, err := server.New(server.Config{DataFile: filepath.Join(dir, "server.json"), RemoteToken: remoteToken, DeviceID: "dev_1", DeviceToken: deviceToken})
	if err != nil {
		t.Fatal(err)
	}
	cloud := httptest.NewServer(gateway.Handler())
	defer cloud.Close()
	adapterFile := filepath.Join(dir, "adapters.enc")
	c, err := connector.New(connector.Config{ServerURL: cloud.URL, DeviceID: "dev_1", DeviceToken: deviceToken, JournalFile: filepath.Join(dir, "journal.enc"), JournalKey: journalKey, AdapterFile: adapterFile})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() { _ = c.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	adapter := connector.AdapterConfig{ID: "third", Type: "openai_compatible", Name: "Third party", Provider: "deepseek", BaseURL: provider.URL + "/api/v1", APIKey: providerKey, Models: []string{"test-model"}, ContextWindow: 128000, MaxOutputTokens: 1024, ReasoningEffort: "medium", Enabled: true}
	created, err := c.CreateAdapter(adapter)
	if err != nil {
		t.Fatal(err)
	}
	if !created.HasAPIKey {
		t.Fatalf("credential leaked or missing flag: %#v", created)
	}
	if !created.IsDefault || created.Provider != "deepseek" || created.ContextWindow != 128000 || created.MaxOutputTokens != 1024 || created.ReasoningEffort != "medium" {
		t.Fatalf("profile settings not persisted in view: %#v", created)
	}
	public, _ := json.Marshal(c.ListAdapters())
	if bytes.Contains(public, []byte(providerKey)) {
		t.Fatal("API key returned from management API")
	}
	if _, err := c.CreateAdapter(connector.AdapterConfig{ID: "ollama", Type: "ollama_http", BaseURL: provider.URL, Models: []string{"ollama-test"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(adapterFile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(providerKey)) {
		t.Fatal("API key stored in plaintext")
	}
	ready := false
	for time.Now().Before(deadline) {
		var models struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		code := api(t, cloud.Client(), cloud.URL, "GET", "/v1/models", remoteToken, nil, &models)
		ids := map[string]bool{}
		for _, m := range models.Data {
			ids[m.ID] = true
		}
		if code == 200 && len(models.Data) == 2 && ids["third/test-model"] && ids["ollama/ollama-test"] {
			ready = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		var state map[string]any
		api(t, cloud.Client(), cloud.URL, "GET", "/v1/connectors", remoteToken, nil, &state)
		t.Fatalf("managed model not advertised to remote service: %#v, local %#v", state, c.ListAdapters())
	}
	body := map[string]any{"model": "third/test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}}}
	reqBody, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", cloud.URL+"/v1/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer "+remoteToken)
	client := &http.Client{Timeout: 10 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !bytes.Contains(data, []byte("Hello!")) {
		t.Fatalf("completion: %d %s", res.StatusCode, data)
	}
	if calls.Load() != 1 {
		t.Fatalf("provider calls: %d", calls.Load())
	}
	body["stream"] = true
	reqBody, _ = json.Marshal(body)
	req, _ = http.NewRequest("POST", cloud.URL+"/v1/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer "+remoteToken)
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ = io.ReadAll(res.Body)
	if res.StatusCode != 200 || !bytes.Contains(data, []byte(`"content":"Hello"`)) || !bytes.Contains(data, []byte("data: [DONE]")) {
		t.Fatalf("stream: %d %s", res.StatusCode, data)
	}
}

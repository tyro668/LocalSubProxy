package connector_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"localsubproxy/internal/connector"
	"localsubproxy/internal/server"
)

func TestManagedAnthropicAdapterEndToEnd(t *testing.T) {
	const providerKey = "anthropic-secret-never-leave-device"
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != providerKey || r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("Authorization") != "" {
			t.Errorf("incorrect Anthropic authentication headers: %s", r.URL.Path)
			http.Error(w, "bad provider headers", 401)
			return
		}
		switch r.URL.Path {
		case "/proxy/v1/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"claude-test"}]}`)
		case "/proxy/v1/messages":
			calls.Add(1)
			var payload struct {
				Model     string `json:"model"`
				System    string `json:"system"`
				Stream    bool   `json:"stream"`
				MaxTokens int    `json:"max_tokens"`
				Messages  []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Model != "claude-test" || payload.System != "Be concise" || !payload.Stream || payload.MaxTokens != 512 || len(payload.Messages) != 1 || payload.Messages[0].Role != "user" || payload.Messages[0].Content != "hi" {
				t.Errorf("incorrect Anthropic Messages request: %#v, %v", payload, err)
				http.Error(w, "bad request", 400)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
			_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"!\"}}\n\n")
			_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
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
	c, err := connector.New(connector.Config{ServerURL: cloud.URL, DeviceID: "dev_1", DeviceToken: deviceToken, JournalFile: filepath.Join(dir, "journal.enc"), JournalKey: journalKey, AdapterFile: filepath.Join(dir, "adapters.enc")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() { _ = c.Run(ctx); close(done) }()
	adapter := connector.AdapterConfig{ID: "anthropic", Type: "anthropic_compatible", Name: "Anthropic", BaseURL: provider.URL + "/proxy", APIKey: providerKey, Models: []string{"claude-test"}, MaxOutputTokens: 512, Enabled: true}
	created, err := c.CreateAdapter(adapter)
	if err != nil {
		t.Fatal(err)
	}
	if !created.HasAPIKey {
		t.Fatal("API key flag missing")
	}
	public, _ := json.Marshal(c.ListAdapters())
	if bytes.Contains(public, []byte(providerKey)) {
		t.Fatal("API key returned from management API")
	}
	updated := adapter
	updated.APIKey = ""
	updated.Provider = "anthropic"
	if _, err := c.UpdateAdapter(updated); err != nil {
		t.Fatalf("saved key not retained on edit: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		var models struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		code := api(t, cloud.Client(), cloud.URL, "GET", "/v1/models", remoteToken, nil, &models)
		if code == 200 && len(models.Data) == 1 && models.Data[0].ID == "anthropic/claude-test" {
			ready = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("Anthropic model not advertised: %#v", c.ListAdapters())
	}
	input := map[string]any{"model": "anthropic/claude-test", "messages": []map[string]string{{"role": "system", "content": "Be concise"}, {"role": "user", "content": "hi"}}, "stream": true}
	body, _ := json.Marshal(input)
	req, _ := http.NewRequest(http.MethodPost, cloud.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+remoteToken)
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !bytes.Contains(data, []byte(`"content":"Hello"`)) || !bytes.Contains(data, []byte(`"content":"!"`)) || !strings.Contains(string(data), "data: [DONE]") {
		t.Fatalf("stream: %d %s", res.StatusCode, data)
	}
	if calls.Load() != 1 {
		t.Fatalf("provider calls: %d", calls.Load())
	}
}

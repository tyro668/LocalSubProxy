package connector

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"localsubproxy/internal/protocol"
	"localsubproxy/internal/server"
)

func TestWSSAndHTTPSFallback(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		name := "wss"
		if fallback {
			name = "https_fallback"
		}
		t.Run(name, func(t *testing.T) {
			var modelCalls, upgrades, httpControls atomic.Int32
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/tags":
					fmt.Fprint(w, `{"models":[{"name":"test-model"}]}`)
				case "/api/chat":
					modelCalls.Add(1)
					fmt.Fprintln(w, `{"message":{"content":"answer"},"done":false}`)
					fmt.Fprintln(w, `{"done":true}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer model.Close()
			dir := t.TempDir()
			s, err := server.New(server.Config{DataFile: filepath.Join(dir, "server.json"), SAASToken: "saas-token-0123456789-0123456789-abcd", DeviceID: "dev_1", DeviceToken: "device-token-0123456789-0123456789-abc"})
			if err != nil {
				t.Fatal(err)
			}
			handler := s.Handler()
			cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/connector/connect" {
					upgrades.Add(1)
					if fallback {
						http.Error(w, "upgrade blocked", http.StatusForbidden)
						return
					}
				}
				if r.URL.Path == "/v1/connector/accepted" || r.URL.Path == "/v1/connector/start" || r.URL.Path == "/v1/connector/events" {
					httpControls.Add(1)
				}
				handler.ServeHTTP(w, r)
			}))
			defer cloud.Close()
			c, err := New(Config{ServerURL: cloud.URL, DeviceID: "dev_1", DeviceToken: "device-token-0123456789-0123456789-abc", AdapterID: "ollama-local", OllamaURL: model.URL, AllowedModels: []string{"test-model"}, JournalFile: filepath.Join(dir, "journal.enc"), JournalKey: "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"})
			if err != nil {
				t.Fatal(err)
			}
			pool := x509.NewCertPool()
			pool.AddCert(cloud.Certificate())
			c.client = cloud.Client()
			c.wsDialer = &websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: 3 * time.Second, Subprotocols: []string{protocol.Version}, TLSClientConfig: &tls.Config{RootCAs: pool}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ended := make(chan struct{})
			go func() { defer close(ended); _ = c.Run(ctx) }()
			deadline := time.Now().Add(6 * time.Second)
			for time.Now().Before(deadline) {
				if upgrades.Load() > 0 && (fallback || c.currentWS() != nil) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if upgrades.Load() == 0 {
				t.Fatal("no WSS attempt")
			}
			req := protocol.CreateTask{Target: protocol.Target{DeviceID: "dev_1", AdapterID: "ollama-local", ModelID: "test-model"}, Operation: "chat.generate", Input: protocol.Input{Messages: []protocol.Message{{Role: "user", Content: "hi"}}}}
			b, _ := json.Marshal(req)
			httpReq, _ := http.NewRequest(http.MethodPost, cloud.URL+"/v1/llm-tasks", bytes.NewReader(b))
			httpReq.Header.Set("Authorization", "Bearer saas-token-0123456789-0123456789-abcd")
			httpReq.Header.Set("Idempotency-Key", "wss-test-key-123")
			resp, err := cloud.Client().Do(httpReq)
			if err != nil {
				t.Fatal(err)
			}
			var task protocol.Task
			if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 202 {
				t.Fatalf("create: %d", resp.StatusCode)
			}
			deadline = time.Now().Add(8 * time.Second)
			for time.Now().Before(deadline) {
				get, _ := http.NewRequest(http.MethodGet, cloud.URL+"/v1/llm-tasks/"+task.ID, nil)
				get.Header.Set("Authorization", "Bearer saas-token-0123456789-0123456789-abcd")
				r, err := cloud.Client().Do(get)
				if err != nil {
					t.Fatal(err)
				}
				_ = json.NewDecoder(r.Body).Decode(&task)
				r.Body.Close()
				if task.State == "SUCCEEDED" {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if task.State != "SUCCEEDED" || task.AckSeq != 3 || modelCalls.Load() != 1 {
				t.Fatalf("task=%s ack=%d model_calls=%d", task.State, task.AckSeq, modelCalls.Load())
			}
			if !fallback && httpControls.Load() != 0 {
				t.Fatalf("WSS used %d HTTP controls", httpControls.Load())
			}
			if fallback && httpControls.Load() == 0 {
				t.Fatal("long-poll fallback did not use HTTPS controls")
			}
			cancel()
			select {
			case <-ended:
			case <-time.After(3 * time.Second):
				t.Fatal("connector did not stop")
			}
		})
	}
}

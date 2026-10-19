package native

import (
	"encoding/json"
	"github.com/tyro668/avora/internal/backend"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestProviderModelsSharedAPI(t *testing.T) {
	startKernel(t, t.TempDir())
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected model request: %s %s, auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"z-model"},{"id":"a-model"},{"id":"z-model"}]}`))
	}))
	defer provider.Close()
	// Discovery works before choosing a model, without saving the draft.
	request, err := json.Marshal(map[string]any{
		"action": "provider_models",
		"adapter": map[string]any{
			"type": "openai_compatible", "base_url": provider.URL + "/v1", "api_key": "test-key",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		OK    bool     `json:"ok"`
		Error string   `json:"error"`
		Data  []string `json:"data"`
	}
	if err := json.Unmarshal([]byte(Call(string(request))), &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || !slices.Equal(response.Data, []string{"a-model", "z-model"}) {
		t.Fatalf("model discovery: %+v", response)
	}
	var catalogRequest map[string]any
	_ = json.Unmarshal(request, &catalogRequest)
	catalogRequest["action"] = "provider_catalog"
	encoded, _ := json.Marshal(catalogRequest)
	var catalog struct {
		OK    bool                `json:"ok"`
		Error string              `json:"error"`
		Data  []backend.ModelInfo `json:"data"`
	}
	if err := json.Unmarshal([]byte(Call(string(encoded))), &catalog); err != nil {
		t.Fatal(err)
	}
	if !catalog.OK || len(catalog.Data) != 2 || catalog.Data[0].ID != "a-model" || catalog.Data[1].ID != "z-model" || catalog.Data[0].ReasoningKnown || len(catalog.Data[0].ReasoningEfforts) != 0 {
		t.Fatalf("catalog fallback: %+v", catalog)
	}
	if adapters := must(t, map[string]any{"action": "list_adapters"})["adapters"].([]any); len(adapters) != 0 {
		t.Fatalf("discovery saved a draft adapter: %v", adapters)
	}
}

func TestOllamaSchemeSharedAPI(t *testing.T) {
	startKernel(t, t.TempDir())
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" { // context window detection: this server offers none
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/api/tags" || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected Ollama model request: %s %s, auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"qwen3:8b","model":"qwen3:8b"},{"model":"llama3:latest"}]}`))
	}))
	defer provider.Close()
	// Discover installed models before selecting one, without an API key.
	request, err := json.Marshal(map[string]any{
		"action":  "provider_models",
		"adapter": map[string]any{"type": "ollama_http", "base_url": provider.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		OK    bool     `json:"ok"`
		Error string   `json:"error"`
		Data  []string `json:"data"`
	}
	if err := json.Unmarshal([]byte(Call(string(request))), &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || !slices.Equal(response.Data, []string{"llama3:latest", "qwen3:8b"}) {
		t.Fatalf("Ollama model discovery: %+v", response)
	}
	adapter := map[string]any{
		"id": "local-ollama", "name": "本机模型", "type": "ollama_http", "provider": "ollama",
		"base_url": provider.URL, "models": []string{"qwen3:8b"}, "enabled": true,
	}
	if health := must(t, map[string]any{"action": "test_adapter", "adapter": adapter}); health["health"] != "ready" {
		t.Fatalf("Ollama test connection: %v", health)
	}
	must(t, map[string]any{"action": "create_adapter", "adapter": adapter})
	adapters := must(t, map[string]any{"action": "list_adapters"})["adapters"].([]any)
	if len(adapters) != 1 {
		t.Fatalf("saved Ollama adapters: %v", adapters)
	}
	saved := adapters[0].(map[string]any)
	if saved["type"] != "ollama_http" || saved["base_url"] != provider.URL || saved["models"].([]any)[0] != "qwen3:8b" {
		t.Fatalf("saved Ollama configuration: %v", saved)
	}
}

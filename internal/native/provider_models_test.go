package native

import (
	"encoding/json"
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
	if adapters := must(t, map[string]any{"action": "list_adapters"})["adapters"].([]any); len(adapters) != 0 {
		t.Fatalf("discovery saved a draft adapter: %v", adapters)
	}
}

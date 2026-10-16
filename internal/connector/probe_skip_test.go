package connector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// Review9 P2-9: a service that runs inference but has no model list endpoint
// is unavailable while the probe runs (and says how to fix it); with
// skip_model_probe saved through the normal create path it is ready.
func TestNoModelListEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer srv.Close()
	x, err := New(Config{MaxConcurrency: 1, DataKey: codexTestDataKey, AdapterFile: filepath.Join(t.TempDir(), "a.enc")})
	if err != nil {
		t.Fatal(err)
	}
	defer x.Shutdown()
	a := AdapterConfig{ID: "nolist", Type: "openai_compatible", BaseURL: srv.URL, APIKey: "k", Models: []string{"m"}, Enabled: true}
	h := x.probeAdapter(context.Background(), a)
	if h.Health != "unavailable" || !strings.Contains(h.Error, "skip model list probe") {
		t.Fatalf("probe without /models: %+v", h)
	}
	a.SkipModelProbe = true
	if _, err := x.CreateAdapter(a); err != nil {
		t.Fatal(err)
	}
	x.mu.Lock()
	saved := x.adapters["nolist"]
	x.mu.Unlock()
	if !saved.SkipModelProbe {
		t.Fatal("skip_model_probe not stored")
	}
	if h := x.probeAdapter(context.Background(), saved); h.Health != "ready" {
		t.Fatalf("with skip_model_probe: %+v", h)
	}
}

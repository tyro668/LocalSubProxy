package connector_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/connector"
)

// P1-2: a single Ollama adapter goes offline (model list empties, the
// gateway is told) and then recovers; the recovery (empty -> non-empty)
// must also send models.update so the gateway lists the model again.
func TestModelsAllOfflineThenRecover(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var down atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{{"name": "llama3"}}})
	}))
	defer srv.Close()
	kick, rounds := make(chan struct{}), make(chan struct{}, 16)
	defer connector.SetHealthLoop(kick, func() { rounds <- struct{}{} })()
	round := func() { // run one full probe round, deterministically
		kick <- struct{}{}
		select {
		case <-rounds:
		case <-ctx.Done():
			t.Fatal("health round did not finish")
		}
	}
	a := newSaaS(t, ctx, "acme")
	c, err := connector.New(connector.Config{DataKey: dataKey, AdapterFile: filepath.Join(t.TempDir(), "adapters.enc"), MaxConcurrency: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateAdapter(connector.AdapterConfig{ID: "ol", Type: "ollama_http", BaseURL: srv.URL, Models: []string{"llama3"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddLink(connector.LinkConfig{ID: "ba", Identity: a.id, Policy: &connector.Policy{Allow: []string{"*"}}}); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer func() { stop(); c.Shutdown() }()
	c.Start(runCtx)
	<-rounds // baseline sample
	a.waitModels(t, ctx, "ol/llama3")

	down.Store(true)
	round()
	if len(c.Models()) != 0 {
		t.Fatalf("models while down: %v", c.Models())
	}
	waitFor(t, "gateway drops the model", func() bool {
		_, b := a.call(t, ctx, "GET", "/v1/models", nil)
		return !strings.Contains(b, "ol/llama3")
	})

	down.Store(false)
	round()
	a.waitModels(t, ctx, "ol/llama3") // fails without the empty->non-empty notify
}

// Review5 P2-c: the hello goes out with the unprobed list, and only then
// does the first probe find the model down; that must be announced.
func TestHelloFirstProbeLater(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var down atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{{"name": "llama3"}}})
	}))
	defer srv.Close()
	gate, rounds := make(chan struct{}), make(chan struct{}, 16)
	var once atomic.Bool
	defer connector.SetHealthBefore(func() {
		if once.CompareAndSwap(false, true) {
			<-gate // hold the first probe until the hello is in
		}
	})()
	defer connector.SetHealthLoop(nil, func() { rounds <- struct{}{} })()
	a := newSaaS(t, ctx, "acme")
	c, err := connector.New(connector.Config{DataKey: dataKey, AdapterFile: filepath.Join(t.TempDir(), "adapters.enc"), MaxConcurrency: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateAdapter(connector.AdapterConfig{ID: "ol", Type: "ollama_http", BaseURL: srv.URL, Models: []string{"llama3"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddLink(connector.LinkConfig{ID: "ba", Identity: a.id, Policy: &connector.Policy{Allow: []string{"*"}}}); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer func() { stop(); c.Shutdown() }()
	c.Start(runCtx)
	a.waitModels(t, ctx, "ol/llama3") // hello carried the unprobed list
	down.Store(true)                  // the model goes down before the first probe
	close(gate)
	<-rounds
	waitFor(t, "gateway drops the model found down by the first probe", func() bool {
		_, b := a.call(t, ctx, "GET", "/v1/models", nil)
		return !strings.Contains(b, "ol/llama3")
	})
}

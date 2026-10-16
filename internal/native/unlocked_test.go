package native

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Review4 B: a slow network model test / Codex model listing must not hold
// the global lock: status answers promptly meanwhile.
func TestStatusPromptWhileSlowCallsRun(t *testing.T) {
	setupBindings(t, bA)
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	var wg sync.WaitGroup
	oc := codexModels
	codexModels = func(ctx context.Context, _ string) ([]string, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, ctx.Err()
	}
	defer func() { close(release); wg.Wait(); codexModels = oc }()
	request := map[string]any{"action": "test_adapter", "adapter": map[string]any{
		"id": "t", "type": "openai_compatible", "name": "t", "base_url": slow.URL + "/v1", "api_key": "k", "models": []string{"m"}}}
	b, _ := json.Marshal(request)
	request["action"] = "provider_models"
	models, _ := json.Marshal(request)
	wg.Add(3)
	go func() { defer wg.Done(); call(string(b)) }()
	go func() { defer wg.Done(); call(string(models)) }()
	go func() { defer wg.Done(); call(`{"action":"codex_models","path":"/bin/false"}`) }()
	time.Sleep(200 * time.Millisecond) // the slow calls are now waiting
	start := time.Now()
	for i := 0; i < 5; i++ {
		must(t, map[string]any{"action": "status"})
		must(t, map[string]any{"action": "list_adapters"})
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("status blocked behind slow calls: %v", d)
	}
}

// Review4 G: start(A) → stop → start(B) in one process uses B's audit.db.
func TestRestartWithOtherDataDirUsesItsAuditDB(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	startKernel(t, a)
	first := runtime.audit
	must(t, map[string]any{"action": "stop"})
	if runtime.audit != nil {
		t.Fatal("stop kept the audit log open")
	}
	startKernel(t, b)
	defer must(t, map[string]any{"action": "stop"})
	if runtime.audit == nil || runtime.audit == first {
		t.Fatal("audit log not reopened for the new data dir")
	}
	if _, err := os.Stat(filepath.Join(b, "audit.db")); err != nil {
		t.Fatalf("B has no audit.db: %v", err)
	}
}

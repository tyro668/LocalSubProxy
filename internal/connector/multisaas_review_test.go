package connector_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyro668/avora/internal/connector"
	"github.com/tyro668/avora/mock/fakemodel"
)

// slowModel answers normally, except prompts containing "slow", which
// stream one chunk and then hang until the request is cancelled.
type slowModel struct {
	fast      http.Handler
	started   atomic.Int32
	cancelled atomic.Int32
}

func (m *slowModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(b))
	if !strings.Contains(string(b), "slow") {
		m.fast.ServeHTTP(w, r)
		return
	}
	m.started.Add(1)
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"thinking\"}}]}\n\n")
	w.(http.Flusher).Flush()
	select {
	case <-r.Context().Done():
		m.cancelled.Add(1)
	case <-time.After(30 * time.Second):
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func connRunning(c *connector.Connector, id string) int { return c.ConnectionRunning(id) }

func setupTwo(t *testing.T, ctx context.Context, polA *connector.Policy) (*connector.Connector, *saas, *saas, *slowModel) {
	m := &slowModel{fast: (&fakemodel.Server{Models: []string{"m1"}}).Handler()}
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	a, b := newSaaS(t, ctx, "acme"), newSaaS(t, ctx, "beta")
	dir := t.TempDir()
	c, err := connector.New(connector.Config{DataKey: dataKey, AdapterFile: filepath.Join(dir, "adapters.enc"), MaxConcurrency: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateAdapter(connector.AdapterConfig{ID: "lm", Type: "openai_compatible", BaseURL: srv.URL, APIKey: "k", Models: []string{"m1"}, Enabled: true, SkipModelProbe: true, Concurrency: 8}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddLink(connector.LinkConfig{ID: "ba", Identity: a.id, Policy: polA}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddLink(connector.LinkConfig{ID: "bb", Identity: b.id, Policy: &connector.Policy{Allow: []string{"*"}}}); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	t.Cleanup(func() { stop(); c.Shutdown() })
	c.Start(runCtx)
	a.waitModels(t, ctx, "lm/m1")
	b.waitModels(t, ctx, "lm/m1")
	return c, a, b, m
}

func slowChat(t *testing.T, ctx context.Context, s *saas) {
	body := `{"model":"` + s.id.DeviceID + `/lm/m1","messages":[{"role":"user","content":"slow please"}]}`
	go func() { // errors are expected (the task is cancelled); never t.Fatal here
		cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(cctx, "POST", s.gw.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+s.key)
		req.Header.Set("X-End-User", "alice")
		if r, err := http.DefaultClient.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, r.Body)
			r.Body.Close()
		}
	}()
}

// Review #2: revoking A at its gateway cancels A's in-flight inference,
// frees its slots, and B keeps working.
func TestRevokeCancelsInflightTasks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, a, b, m := setupTwo(t, ctx, &connector.Policy{Allow: []string{"*"}})
	slowChat(t, ctx, a)
	waitFor(t, "A's task to start", func() bool { return connRunning(c, "ba") == 1 && m.started.Load() == 1 })
	a.call(t, ctx, "POST", "/lmbp/v1/tenant/devices/"+a.id.DeviceID+"/revoke", nil)
	waitFor(t, "A's task to be cancelled", func() bool { return m.cancelled.Load() == 1 && connRunning(c, "ba") == 0 })
	waitFor(t, "A revoked", func() bool { return c.Status()["connections"].([]map[string]any)[0]["state"] == "revoked" })
	if n := c.Status()["running_tasks"]; n != 0 {
		t.Fatalf("slots still held: %v", n)
	}
	if code, body := b.chat(t, ctx, "lm/m1"); code != 200 {
		t.Fatalf("beta after A revoked: %d %s", code, body)
	}
	// removal after revoke is clean (no running tasks left)
	if err := c.RemoveLink("ba"); err != nil {
		t.Fatal(err)
	}
}

// Review #2: Shutdown (permanent stop) cancels in-flight tasks and waits.
func TestShutdownCancelsInflightTasks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, a, _, m := setupTwo(t, ctx, &connector.Policy{Allow: []string{"*"}})
	slowChat(t, ctx, a)
	waitFor(t, "task start", func() bool { return connRunning(c, "ba") == 1 && m.started.Load() == 1 })
	done := make(chan struct{})
	go func() { c.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return: in-flight task not cancelled")
	}
	if connRunning(c, "ba") != 0 {
		t.Fatalf("task not cleaned up: running=%d", connRunning(c, "ba"))
	}
	// the model server observes the client disconnect asynchronously
	waitFor(t, "upstream request cancelled", func() bool { return m.cancelled.Load() == 1 })
}

// Review #2: once removal begins no new task is admitted on that link.
func TestBeginRemoveRefusesBusyAndNewTasks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, a, b, m := setupTwo(t, ctx, &connector.Policy{Allow: []string{"*"}})
	slowChat(t, ctx, a)
	waitFor(t, "task start", func() bool { return connRunning(c, "ba") == 1 && m.started.Load() == 1 })
	if err := c.BeginRemove("ba"); err == nil {
		t.Fatal("BeginRemove must refuse while tasks run")
	}
	if err := c.BeginRemove("bb"); err != nil {
		t.Fatal(err)
	}
	if code, _ := b.chat(t, ctx, "lm/m1"); code == 200 {
		t.Fatal("task admitted on a closing connection")
	}
	c.AbortRemove("bb")
	if code, body := b.chat(t, ctx, "lm/m1"); code != 200 {
		t.Fatalf("after AbortRemove: %d %s", code, body)
	}
}

// Review #5: raising the per-connection cap from 1 to 2 while a task runs
// applies to the live session (gateway and device) immediately.
func TestConcurrencyRaisedDuringTask(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, a, _, m := setupTwo(t, ctx, &connector.Policy{Allow: []string{"*"}, MaxConcurrency: 1})
	slowChat(t, ctx, a)
	waitFor(t, "first task", func() bool { return connRunning(c, "ba") == 1 })
	if code, _ := a.chat(t, ctx, "lm/m1"); code == 200 {
		t.Fatal("cap 1 allowed a second task")
	}
	if err := c.SetPolicy("ba", connector.Policy{Allow: []string{"*"}, MaxConcurrency: 2}); err != nil {
		t.Fatal(err)
	}
	if mc := c.Status()["connections"].([]map[string]any)[0]["max_concurrency"]; mc != 2 {
		t.Fatalf("status max_concurrency %v", mc)
	}
	var code int
	var body string
	for i := 0; i < 50; i++ { // models.update reaches the gateway asynchronously
		if code, body = a.chat(t, ctx, "lm/m1"); code == 200 {
			break
		}
		t.Logf("attempt %d: %d %.200s", i, code, body)
		time.Sleep(100 * time.Millisecond)
	}
	if code != 200 {
		t.Fatalf("raised cap not applied while the first task runs: %d %s", code, body)
	}
	if m.cancelled.Load() != 0 || connRunning(c, "ba") != 1 {
		t.Fatal("the running task was disturbed")
	}
}

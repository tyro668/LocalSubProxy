package client_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp/client"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/gateway"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/store"
)

// Review2 #1: the user changes concurrency 1→2 while the client waits for
// the welcome. The welcome echoes the stale hello (1); the client must keep
// 2 locally and announce 2 to the gateway.
type gwEnv struct {
	call func(method, path string, body any) string
	try  func(method, path string, body any) (string, error) // no t.Fatal: safe in goroutines
	id   client.Identity
}

func newGW(t *testing.T, ctx context.Context) gwEnv { return newGWCfg(t, ctx, nil) }

func newGWCfg(t *testing.T, ctx context.Context, mod func(*gateway.Config)) gwEnv {
	st, _ := store.OpenSQLite(":memory:", nil)
	t.Cleanup(func() { st.Close() })
	cfg := gateway.Config{Store: st, Logger: log.New(io.Discard, "", 0)}
	if mod != nil {
		mod(&cfg)
	}
	gw, _ := gateway.New(cfg)
	key := strings.Repeat("k", 40)
	_ = gw.RegisterTenant(ctx, "acme", "ACME", key)
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	try := func(method, path string, body any) (string, error) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(ctx, method, srv.URL+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-End-User", "alice")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		defer r.Body.Close()
		out, _ := io.ReadAll(r.Body)
		return string(out), nil
	}
	call := func(method, path string, body any) string {
		out, err := try(method, path, body)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"gateway_url": srv.URL, "tenant": "acme", "link_url": "http://127.0.0.1/link"})
	}))
	t.Cleanup(remote.Close)
	p, err := client.StartPairing(ctx, nil, remote.URL, "dev")
	if err != nil {
		t.Fatal(err)
	}
	call("POST", "/lmbp/v1/tenant/devices/approvals", map[string]string{"user_code": p.UserCode})
	id, err := p.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return gwEnv{call: call, try: try, id: id}
}

func (g gwEnv) waitConc(t *testing.T, want int) {
	t.Helper()
	w := fmt.Sprintf(`"max_concurrency":%d`, want)
	for i := 0; ; i++ {
		if strings.Contains(g.call("GET", "/lmbp/v1/tenant/devices", nil), w) {
			return
		}
		if i > 100 {
			t.Fatalf("gateway never learned %d: %s", want, g.call("GET", "/lmbp/v1/tenant/devices", nil))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Review3 #1: an announcement built with limit 2 is delayed before it is
// sent; meanwhile the user sets 3. The stale 2 must not be the last word.
func TestConcurrencyAnnouncementsDoNotReorder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	g := newGW(t, ctx)
	c, err := client.New(client.Config{Executor: &client.OpenAIExecutor{ModelList: []string{"m"}}, MaxConcurrency: 1, Logger: log.New(io.Discard, "", 0)}, g.id)
	if err != nil {
		t.Fatal(err)
	}
	rctx, rcancel := context.WithCancel(ctx)
	defer rcancel()
	ready := make(chan error, 1)
	go c.Run(rctx, ready)
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	built2 := make(chan struct{})
	var once sync.Once
	client.SetAnnounceHook(func(b []byte) {
		if strings.Contains(string(b), `"max_concurrency":2`) {
			once.Do(func() { close(built2) })
			time.Sleep(300 * time.Millisecond) // the stale message is slow to go out
		}
	})
	defer client.SetAnnounceHook(nil)
	done := make(chan struct{})
	go func() { c.SetMaxConcurrency(2); close(done) }()
	<-built2
	c.SetMaxConcurrency(3) // the user's newer setting
	<-done
	time.Sleep(200 * time.Millisecond)
	g.waitConc(t, 3)
	if d, e := c.Concurrency(); d != 3 || e != 3 {
		t.Fatalf("client %d/%d", d, e)
	}
}

func TestConcurrencyChangedDuringHandshake(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	g := newGW(t, ctx)
	call, id := g.call, g.id
	_ = call
	var err error
	var c *client.Client
	release := make(chan struct{})
	client.SetHandshakeHook(func() {
		go c.SetMaxConcurrency(2) // user edit while awaiting the welcome
		time.Sleep(200 * time.Millisecond)
		close(release)
	})
	defer client.SetHandshakeHook(nil)
	c, err = client.New(client.Config{Executor: &client.OpenAIExecutor{ModelList: []string{"m"}}, MaxConcurrency: 1, Logger: log.New(io.Discard, "", 0)}, id)
	if err != nil {
		t.Fatal(err)
	}
	rctx, rcancel := context.WithCancel(ctx)
	defer rcancel()
	ready := make(chan error, 1)
	go c.Run(rctx, ready)
	<-release
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	if d, e := c.Concurrency(); d != 2 || e != 2 {
		t.Fatalf("after echoed welcome: declared %d effective %d (want 2/2)", d, e)
	}
	for i := 0; ; i++ {
		if strings.Contains(call("GET", "/lmbp/v1/tenant/devices", nil), `"max_concurrency":2`) {
			break
		}
		if i > 100 {
			t.Fatalf("gateway never learned 2: %s", call("GET", "/lmbp/v1/tenant/devices", nil))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

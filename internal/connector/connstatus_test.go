package connector_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/audit"
	"github.com/tyro668/LocalSubProxy/internal/connector"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/client"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/gateway"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/store"
	"github.com/tyro668/LocalSubProxy/mock/fakemodel"
)

// The SaaS origin (different from the gateway) does not serve the OPTIONAL
// /.well-known/localsubproxy endpoint (lmbp-spec: RECOMMENDED). Online must
// follow the LMBP session alone; discovery is display-only.
func TestOnlineWithoutWellKnownOnOtherOrigin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	model := httptest.NewServer((&fakemodel.Server{Models: []string{"m1"}}).Handler())
	defer model.Close()
	st, _ := store.OpenSQLite(":memory:", nil)
	defer st.Close()
	gw, _ := gateway.New(gateway.Config{Store: st, Logger: log.New(io.Discard, "", 0)})
	key := strings.Repeat("k", 40)
	_ = gw.RegisterTenant(ctx, "acme", "Acme", key)
	gws := httptest.NewServer(gw.Handler())
	defer gws.Close()
	pairSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"gateway_url": gws.URL, "tenant": "acme", "link_url": "http://127.0.0.1/link"})
	}))
	defer pairSrv.Close()
	saas := httptest.NewServer(http.NotFoundHandler()) // no well-known endpoint
	defer saas.Close()
	tenant := func(method, path string, body any) *http.Response {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(ctx, method, gws.URL+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-End-User", "alice")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	p, err := client.StartPairing(ctx, nil, pairSrv.URL, connector.DeviceName())
	if err != nil {
		t.Fatal(err)
	}
	if r := tenant("POST", "/lmbp/v1/tenant/devices/approvals", map[string]string{"user_code": p.UserCode}); r.StatusCode != 200 {
		t.Fatalf("approve: %d", r.StatusCode)
	}
	id, err := p.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	al, _ := audit.Open(filepath.Join(dir, "audit.db"))
	defer al.Close()
	c, err := connector.New(connector.Config{Identity: &id, RemoteURL: saas.URL, DataKey: dataKey, AdapterFile: filepath.Join(dir, "adapters.enc"), Audit: al})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateAdapter(connector.AdapterConfig{ID: "lm", Type: "openai_compatible", BaseURL: model.URL, APIKey: "local", Models: []string{"m1"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = c.Run(runCtx) }()

	var conn map[string]any
	for i := 0; i < 100; i++ {
		s := c.Status()
		cs, _ := s["connections"].([]map[string]any)
		if len(cs) == 1 && cs[0]["state"] == "online" && cs[0]["discovery"] == "unavailable" {
			conn = cs[0]
			if s["connected"] != true || s["connections_online"] != 1 {
				t.Fatalf("flat status disagrees: %v", s)
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if conn == nil {
		t.Fatalf("never online with discovery unavailable: %v", c.Status()["connections"])
	}
	if conn["connected"] != true || conn["remote_connected"] != false {
		t.Fatalf("conn %v", conn)
	}
	r := tenant("POST", "/v1/chat/completions", map[string]any{"model": id.DeviceID + "/lm/m1", "stream": true, "messages": []map[string]string{{"role": "user", "content": "ping"}}})
	defer r.Body.Close()
	sc := bufio.NewScanner(r.Body)
	done := false
	for sc.Scan() {
		if strings.TrimPrefix(sc.Text(), "data: ") == "[DONE]" {
			done = true
			break
		}
	}
	if r.StatusCode != 200 || !done {
		t.Fatalf("task did not complete: %d", r.StatusCode)
	}
}

package connector_test

import (
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

type saas struct {
	gw     *httptest.Server
	remote *httptest.Server
	key    string
	id     client.Identity
}

func (s *saas) call(t *testing.T, ctx context.Context, method, path string, body any) (int, string) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, method, s.gw.URL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+s.key)
	req.Header.Set("X-End-User", "alice")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	out, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(out)
}

// newSaaS starts one gateway with one tenant and pairs a fresh device with it.
func newSaaS(t *testing.T, ctx context.Context, tenant string) *saas {
	st, _ := store.OpenSQLite(":memory:", nil)
	t.Cleanup(func() { st.Close() })
	gw, _ := gateway.New(gateway.Config{Store: st, Logger: log.New(io.Discard, "", 0)})
	s := &saas{key: strings.Repeat(tenant[:1], 40)}
	_ = gw.RegisterTenant(ctx, tenant, strings.ToUpper(tenant), s.key)
	s.gw = httptest.NewServer(gw.Handler())
	t.Cleanup(s.gw.Close)
	s.remote = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"gateway_url": s.gw.URL, "tenant": tenant, "link_url": "http://127.0.0.1/link"})
	}))
	t.Cleanup(s.remote.Close)
	p, err := client.StartPairing(ctx, nil, s.remote.URL, connector.DeviceName())
	if err != nil {
		t.Fatal(err)
	}
	if c, b := s.call(t, ctx, "POST", "/lmbp/v1/tenant/devices/approvals", map[string]string{"user_code": p.UserCode}); c != 200 {
		t.Fatalf("approve %d %s", c, b)
	}
	if s.id, err = p.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *saas) waitModels(t *testing.T, ctx context.Context, want string) string {
	var b string
	for i := 0; i < 100; i++ {
		_, b = s.call(t, ctx, "GET", "/v1/models", nil)
		if strings.Contains(b, want) {
			return b
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("model %s never appeared: %s", want, b)
	return b
}

func (s *saas) chat(t *testing.T, ctx context.Context, model string) (int, string) {
	return s.call(t, ctx, "POST", "/v1/chat/completions", map[string]any{"model": s.id.DeviceID + "/" + model, "messages": []map[string]string{{"role": "user", "content": "ping"}}})
}

// One desktop core serves two SaaS gateways at once, each with its own
// allow-list; disconnecting one leaves the other running.
func TestTwoGatewaysAtOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	model := httptest.NewServer((&fakemodel.Server{Models: []string{"m1", "m2"}}).Handler())
	defer model.Close()
	a, b := newSaaS(t, ctx, "acme"), newSaaS(t, ctx, "beta")
	dir := t.TempDir()
	al, _ := audit.Open(filepath.Join(dir, "audit.db"))
	defer al.Close()
	c, err := connector.New(connector.Config{DataKey: dataKey, AdapterFile: filepath.Join(dir, "adapters.enc"), Audit: al})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateAdapter(connector.AdapterConfig{ID: "lm", Type: "openai_compatible", BaseURL: model.URL, APIKey: "local", Models: []string{"m1", "m2"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddLink(connector.LinkConfig{ID: "ba", Identity: a.id, RemoteURL: a.remote.URL, Policy: &connector.Policy{Allow: []string{"lm/m1"}}}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddLink(connector.LinkConfig{ID: "bb", Identity: b.id, RemoteURL: b.remote.URL, Policy: &connector.Policy{Allow: []string{"lm/*"}}}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddLink(connector.LinkConfig{ID: "bx", Identity: a.id}); err == nil {
		t.Fatal("a second binding to the same gateway+tenant must be rejected")
	}
	runCtx, stop := context.WithCancel(ctx)
	defer func() { stop(); c.Shutdown() }()
	c.Start(runCtx)

	if ms := a.waitModels(t, ctx, "lm/m1"); strings.Contains(ms, "lm/m2") {
		t.Fatalf("acme sees a model outside its allow-list: %s", ms)
	}
	b.waitModels(t, ctx, "lm/m2")
	for i := 0; i < 100 && c.Status()["connections_online"] != 2; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if s := c.Status(); s["connections_online"] != 2 || s["connections_total"] != 2 {
		t.Fatalf("status %v", s["connections"])
	}
	if code, body := a.chat(t, ctx, "lm/m1"); code != 200 || !strings.Contains(body, "ping") {
		t.Fatalf("acme chat %d %s", code, body)
	}
	if code, body := b.chat(t, ctx, "lm/m2"); code != 200 || !strings.Contains(body, "ping") {
		t.Fatalf("beta chat %d %s", code, body)
	}
	if code, _ := a.chat(t, ctx, "lm/m2"); code == 200 {
		t.Fatal("acme called a model outside its allow-list")
	}
	recs, _ := c.AuditList(ctx, audit.Filter{Gateway: b.gw.URL})
	if len(recs) != 1 || recs[0].Tenant != "beta" || recs[0].ConnectionID != b.id.DeviceID {
		t.Fatalf("beta audit %+v", recs)
	}
	if st, _ := c.UsageStats(ctx, audit.Filter{}, []string{audit.BySaaS}, false); len(st) != 2 {
		t.Fatalf("usage by saas %+v", st)
	}

	// Disconnect acme only.
	if err := c.SetPaused("ba", true); err != nil {
		t.Fatal(err)
	}
	if code, body := b.chat(t, ctx, "lm/m1"); code != 200 {
		t.Fatalf("beta must keep working: %d %s", code, body)
	}
	if err := c.RemoveLink("ba"); err != nil {
		t.Fatal(err)
	}
	if s := c.Status(); s["connections_total"] != 1 || s["tenant"] != "beta" {
		t.Fatalf("after remove %v", s)
	}
	// Revoking beta at its gateway marks only that connection revoked.
	b.call(t, ctx, "POST", "/lmbp/v1/tenant/devices/"+b.id.DeviceID+"/revoke", nil)
	var state any
	for i := 0; i < 100; i++ {
		if state = c.Status()["connections"].([]map[string]any)[0]["state"]; state == "revoked" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if state != "revoked" {
		t.Fatalf("state %v", state)
	}
}

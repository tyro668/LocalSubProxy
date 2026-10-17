package connector_test

import (
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

	"github.com/tyro668/LocalSubProxy/internal/connector"
	"github.com/tyro668/LocalSubProxy/internal/durable"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/client"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/gateway"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/store"
	"github.com/tyro668/LocalSubProxy/mock/fakemodel"
)

// Audit2 P2-1: the published model list must satisfy the hello constraints
// (unique, no whitespace, "scheme/model" ≤ 256 code points, ≤ 500 entries).
// Saved schemes that break them load as isolated health errors and are left
// out of hello/models.update, so the SaaS connection still comes up and the
// good schemes are served; new ones are rejected when saved.
func TestPublishedModelsMeetHelloConstraints(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	model := httptest.NewServer((&fakemodel.Server{Models: []string{"m1", "foo"}}).Handler())
	defer model.Close()
	st, _ := store.OpenSQLite(":memory:", nil)
	defer st.Close()
	gw, _ := gateway.New(gateway.Config{Store: st, Logger: log.New(io.Discard, "", 0)})
	key := strings.Repeat("k", 40)
	_ = gw.RegisterTenant(ctx, "acme", "Acme", key)
	gws := httptest.NewServer(gw.Handler())
	defer gws.Close()
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"gateway_url": gws.URL, "tenant": "acme", "link_url": "http://127.0.0.1/link"})
	}))
	defer remote.Close()
	models := func() string {
		req, _ := http.NewRequestWithContext(ctx, "GET", gws.URL+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-End-User", "alice")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return string(b)
	}
	p, err := client.StartPairing(ctx, nil, remote.URL, connector.DeviceName())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"user_code": p.UserCode})
	req, _ := http.NewRequestWithContext(ctx, "POST", gws.URL+"/lmbp/v1/tenant/devices/approvals", strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-End-User", "alice")
	req.Header.Set("Content-Type", "application/json")
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != 200 {
		t.Fatalf("approve: %v %v", r, err)
	}
	id, err := p.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	f := filepath.Join(dir, "adapters.enc")
	oa := func(id string, ms ...string) connector.AdapterConfig {
		return connector.AdapterConfig{ID: id, Type: "openai_compatible", BaseURL: model.URL, APIKey: "local", Models: ms, Enabled: true}
	}
	long := strings.Repeat("s", 64)
	saved := []connector.AdapterConfig{
		oa("lm", "m1"),
		oa("dup", "foo", "foo"),            // duplicate model
		oa(long, strings.Repeat("b", 200)), // 64 + 1 + 200 = 265 code points
		oa("sp", "has space"),              // whitespace
	}
	c, _ := durable.NewCipher(dataKey)
	sealed, _ := c.Seal(saved)
	if err := durable.Write(f, sealed); err != nil {
		t.Fatal(err)
	}
	x, err := connector.New(connector.Config{Identity: &id, RemoteURL: remote.URL, DataKey: dataKey, AdapterFile: f})
	if err != nil {
		t.Fatalf("bad saved schemes failed the load: %v", err)
	}
	bad := map[string]bool{"dup": true, long: true, "sp": true}
	for _, v := range x.ListAdapters() {
		if bad[v.ID] && !strings.Contains(v.HealthError, "invalid configuration") {
			t.Errorf("scheme %q should load as a health error, got %+v", v.ID, v)
		}
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = x.Run(runCtx) }()
	waitFor := func(want string) {
		for i := 0; i < 150; i++ {
			if strings.Contains(models(), want) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("model %s never published; /v1/models = %s; status %v", want, models(), x.Status())
	}
	waitFor(id.DeviceID + "/lm/m1") // the connection is not blocked by the bad schemes
	if m := models(); strings.Contains(m, "/dup/") || strings.Contains(m, long) || strings.Contains(m, "/sp/") {
		t.Fatalf("bad scheme published: %s", m)
	}

	// saving a bad scheme is rejected with the same rules
	for name, a := range map[string]connector.AdapterConfig{
		"duplicate foo,foo":    oa("dup2", "foo", "foo"),
		"64 + 200 = 265 chars": oa(strings.Repeat("t", 64), strings.Repeat("b", 200)),
		"whitespace":           oa("sp2", "a b"),
		"scheme whitespace":    oa("s p", "m1"),
	} {
		if _, err := x.CreateAdapter(a); err == nil {
			t.Errorf("%s: saved", name)
		}
	}
	// still editable: fixing the duplicate publishes it through models.update
	patch, _ := json.Marshal(map[string]any{"models": []string{"foo"}})
	if _, err := x.PatchAdapter("dup", patch); err != nil {
		t.Fatalf("fix dup: %v", err)
	}
	waitFor(id.DeviceID + "/dup/foo")
	// and deletable
	if err := x.DeleteAdapter("sp"); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

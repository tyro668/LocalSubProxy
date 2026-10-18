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

	"github.com/tyro668/avora/internal/connector"
	"github.com/tyro668/avora/internal/durable"
	"github.com/tyro668/avora/internal/lmbp"
	"github.com/tyro668/avora/internal/lmbp/client"
	"github.com/tyro668/avora/internal/lmbp/gateway"
	"github.com/tyro668/avora/internal/lmbp/store"
	"github.com/tyro668/avora/mock/fakemodel"
)

// capdedupe: hello/models.update model_capabilities lists must have unique
// items (defs.json modelCapabilities), or the gateway rejects the whole
// handshake. Saving duplicates is refused; a legacy config that already has
// them loads deduped with a health note, and the device still connects.

func TestDuplicateCapabilitiesRejectedOnSave(t *testing.T) {
	model := httptest.NewServer((&fakemodel.Server{Models: []string{"m1"}}).Handler())
	defer model.Close()
	oa := func(id string, caps ...string) connector.AdapterConfig {
		return connector.AdapterConfig{ID: id, Type: "openai_compatible", BaseURL: model.URL, APIKey: "local", Models: []string{"m1"}, Capabilities: caps, Enabled: true}
	}
	// CLI path: --capabilities tools,tools
	if _, err := connector.New(connector.Config{DataKey: dataKey, Adapters: []connector.AdapterConfig{oa("cli", "tools", "tools")}}); err == nil || !strings.Contains(err.Error(), `duplicate capability "tools"`) {
		t.Errorf("CLI config with tools,tools: got %v, want a duplicate capability error", err)
	}
	x, err := connector.New(connector.Config{DataKey: dataKey, AdapterFile: filepath.Join(t.TempDir(), "adapters.enc")})
	if err != nil {
		t.Fatal(err)
	}
	// API/desktop create
	if _, err := x.CreateAdapter(oa("api", "tools", "vision", "tools")); err == nil || !strings.Contains(err.Error(), `duplicate capability "tools"`) {
		t.Errorf("create with tools,vision,tools: got %v", err)
	}
	// API/desktop edit
	if _, err := x.CreateAdapter(oa("ok", "tools")); err != nil {
		t.Fatal(err)
	}
	patch, _ := json.Marshal(map[string]any{"capabilities": []string{"vision", "vision"}})
	if _, err := x.PatchAdapter("ok", patch); err == nil || !strings.Contains(err.Error(), `duplicate capability "vision"`) {
		t.Errorf("patch to vision,vision: got %v", err)
	}
	for _, v := range x.ListAdapters() {
		if v.ID == "api" {
			t.Errorf("rejected scheme was saved: %+v", v)
		}
		if v.ID == "ok" && strings.Join(v.ConfiguredCapabilities, ",") != "tools" {
			t.Errorf("rejected patch changed the scheme: %+v", v)
		}
	}
}

func TestLegacyDuplicateCapabilitiesLoadDedupedAndConnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
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
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"gateway_url": gws.URL, "tenant": "acme", "link_url": "http://127.0.0.1/link"})
	}))
	defer remote.Close()
	get := func(path string) string {
		req, _ := http.NewRequestWithContext(ctx, "GET", gws.URL+path, nil)
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

	// a config saved by an older version, before duplicates were refused
	f := filepath.Join(t.TempDir(), "adapters.enc")
	c, _ := durable.NewCipher(dataKey)
	sealed, _ := c.Seal([]connector.AdapterConfig{{ID: "lm", Type: "openai_compatible", BaseURL: model.URL, APIKey: "local", Models: []string{"m1"},
		Capabilities: []string{"tools", "vision", "tools"}, Enabled: true}})
	if err := durable.Write(f, sealed); err != nil {
		t.Fatal(err)
	}
	x, err := connector.New(connector.Config{Identity: &id, RemoteURL: remote.URL, DataKey: dataKey, AdapterFile: f})
	if err != nil {
		t.Fatalf("legacy duplicate capabilities failed the load: %v", err)
	}
	for _, v := range x.ListAdapters() {
		if got := strings.Join(v.ConfiguredCapabilities, ","); got != "tools,vision" {
			t.Errorf("capabilities not deduped in order: %s", got)
		}
		if !strings.Contains(v.HealthNote, `duplicate capability "tools" removed`) || v.HealthError != "" {
			t.Errorf("want a health note (not an error) about the dedupe, got note %q error %q", v.HealthNote, v.HealthError)
		}
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = x.Run(runCtx) }()
	want := id.DeviceID + "/lm/m1"
	for i := 0; ; i++ {
		if strings.Contains(get("/v1/models"), want) {
			break
		}
		if i > 150 {
			t.Fatalf("handshake never succeeded: /v1/models = %s; status %v", get("/v1/models"), x.Status())
		}
		time.Sleep(100 * time.Millisecond)
	}
	var dl struct {
		Devices []struct {
			ModelCapabilities map[string][]string `json:"model_capabilities"`
		} `json:"devices"`
	}
	_ = json.Unmarshal([]byte(get("/lmbp/v1/tenant/devices")), &dl)
	if len(dl.Devices) != 1 {
		t.Fatalf("devices: %+v", dl)
	}
	caps := dl.Devices[0].ModelCapabilities["lm/m1"]
	seen := map[string]bool{}
	for _, c := range caps {
		if seen[c] {
			t.Fatalf("published capabilities repeat %q: %v", c, caps)
		}
		seen[c] = true
	}
	if !seen[lmbp.ModelCapTools] || !seen[lmbp.ModelCapVision] {
		t.Fatalf("published capabilities lost a flag: %v", caps)
	}
}

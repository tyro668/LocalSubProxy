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
	"github.com/tyro668/LocalSubProxy/internal/lsp2/client"
	"github.com/tyro668/LocalSubProxy/internal/lsp2/gateway"
	"github.com/tyro668/LocalSubProxy/internal/lsp2/store"
	"github.com/tyro668/LocalSubProxy/mock/fakemodel"
)

// The desktop core pairs over RFC 8628, connects via lsp2 and serves a
// streaming completion through its managed OpenAI-compatible adapter.
const dataKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func TestDesktopCoreEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	model := httptest.NewServer((&fakemodel.Server{Models: []string{"m1", "m2"}}).Handler())
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

	p, err := client.StartPairing(ctx, nil, remote.URL, connector.DeviceName())
	if err != nil {
		t.Fatal(err)
	}
	if r := tenant("POST", "/v2/tenant/device-approvals", map[string]string{"user_code": p.UserCode}); r.StatusCode != 200 {
		t.Fatalf("approve: %d", r.StatusCode)
	}
	id, err := p.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	al, err := audit.Open(filepath.Join(dir, "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer al.Close()
	c, err := connector.New(connector.Config{Identity: &id, RemoteURL: remote.URL, DataKey: dataKey, AdapterFile: filepath.Join(dir, "adapters.enc"), Audit: al})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateAdapter(connector.AdapterConfig{ID: "lm", Type: "openai_compatible", BaseURL: model.URL, APIKey: "local", Models: []string{"m1"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = c.Run(runCtx) }()
	waitModels := func(want string) {
		for i := 0; i < 100; i++ {
			r := tenant("GET", "/v1/models", nil)
			b, _ := io.ReadAll(r.Body)
			r.Body.Close()
			if strings.Contains(string(b), want) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("model %s never appeared", want)
	}
	waitModels(id.DeviceID + "/lm/m1")
	if s := c.Status(); s["protocol"] != "lsp2" || s["gateway_connected"] != true {
		t.Fatalf("status %v", s)
	}

	r := tenant("POST", "/v1/chat/completions", map[string]any{"model": id.DeviceID + "/lm/m1", "stream": true, "messages": []map[string]string{{"role": "user", "content": "ping"}}})
	defer r.Body.Close()
	var text strings.Builder
	done := false
	sc := bufio.NewScanner(r.Body)
	for sc.Scan() {
		line := strings.TrimPrefix(sc.Text(), "data: ")
		if line == "[DONE]" {
			done = true
			break
		}
		var ch struct {
			Choices []struct{ Delta struct{ Content string } }
		}
		if json.Unmarshal([]byte(line), &ch) == nil && len(ch.Choices) > 0 {
			text.WriteString(ch.Choices[0].Delta.Content)
		}
	}
	if !done || !strings.Contains(text.String(), "ping") {
		t.Fatalf("stream done=%v text=%q", done, text.String())
	}

	// The task is in the local audit log, attributed to the SaaS, without content.
	var recs []audit.Record
	for i := 0; i < 50 && len(recs) == 0; i++ {
		recs, _ = c.AuditList(ctx, audit.Filter{})
		time.Sleep(20 * time.Millisecond)
	}
	if len(recs) != 1 || recs[0].Tenant != "acme" || recs[0].Gateway != gws.URL || recs[0].ConnectionID != id.DeviceID || recs[0].Model != "lm/m1" || recs[0].Status != audit.StatusOK {
		t.Fatalf("audit %+v", recs)
	}
	if b, _ := json.Marshal(recs); strings.Contains(string(b), "ping") {
		t.Fatal("audit log contains prompt content")
	}
	if st, err := c.UsageStats(ctx, audit.Filter{}, []string{audit.BySaaS, audit.ByModel}, false); err != nil || len(st) != 1 || st[0].Requests != 1 {
		t.Fatalf("stats %+v %v", st, err)
	}

	// Adapter edits are re-announced on the live session (models.update).
	if _, err := c.UpdateAdapter(connector.AdapterConfig{ID: "lm", Type: "openai_compatible", BaseURL: model.URL, Models: []string{"m1", "m2"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	waitModels(id.DeviceID + "/lm/m2")

	// Revocation stops the session; the core reports the error.
	r2 := tenant("POST", "/v2/tenant/devices/"+id.DeviceID+"/revoke", nil)
	r2.Body.Close()
	for i := 0; i < 100 && c.Status()["gateway_connected"] == true; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if c.Status()["gateway_connected"] == true {
		t.Fatal("still connected after revoke")
	}
}

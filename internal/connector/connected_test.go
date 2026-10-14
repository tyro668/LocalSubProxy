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

	"github.com/tyro668/LocalSubProxy/internal/connector"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/client"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/gateway"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/store"
)

// Regression (macOS + mock gateway): after pairing from an lmbp://pair link
// the app stores the tenant homepage as remote_url. That site does not serve
// the legacy /.well-known/localsubproxy discovery, so status said
// connected=false forever although the device's lmbp session was online and
// serving chats. "connected" is the state of this device's gateway session.
func TestConnectedFollowsGatewaySessionNotRemoteProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, _ := store.OpenSQLite(":memory:", nil)
	defer st.Close()
	gw, _ := gateway.New(gateway.Config{Store: st, Logger: log.New(io.Discard, "", 0)})
	key := strings.Repeat("k", 40)
	_ = gw.RegisterTenant(ctx, "acme", "Acme", key)
	gws := httptest.NewServer(gw.Handler())
	defer gws.Close()
	discovery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"gateway_url": gws.URL, "tenant": "acme", "link_url": "http://127.0.0.1/link"})
	}))
	defer discovery.Close()
	homepage := httptest.NewServer(http.NotFoundHandler()) // like https://acme.example
	defer homepage.Close()

	p, err := client.StartPairing(ctx, nil, discovery.URL, connector.DeviceName())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"user_code": p.UserCode})
	req, _ := http.NewRequestWithContext(ctx, "POST", gws.URL+"/lmbp/v1/tenant/devices/approvals", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-End-User", "alice")
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != 200 {
		t.Fatalf("approve: %v", err)
	}
	id, err := p.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c, err := connector.New(connector.Config{Identity: &id, RemoteURL: homepage.URL, DataKey: dataKey, AdapterFile: filepath.Join(t.TempDir(), "adapters.enc")})
	if err != nil {
		t.Fatal(err)
	}
	if c.Status()["connected"] != false {
		t.Fatal("not connected before Run")
	}
	runCtx, stop := context.WithCancel(ctx)
	go func() { _ = c.Run(runCtx) }()
	deadline := time.Now().Add(10 * time.Second)
	for c.Status()["gateway_connected"] != true && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // let the remote probe run at least once
	if s := c.Status(); s["gateway_connected"] != true || s["connected"] != true {
		t.Fatalf("session is online, status must say connected: %v", s)
	}
	stop()
	for c.Status()["gateway_connected"] == true && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if c.Status()["connected"] != false {
		t.Fatal("connected must drop when the session ends")
	}
}

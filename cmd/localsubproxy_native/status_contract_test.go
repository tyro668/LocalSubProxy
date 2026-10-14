package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/gateway"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/store"
)

type statusContract struct {
	Status          map[string]any `json:"status"`
	Optional        map[string]any `json:"optional"`
	PairStatusState []string       `json:"pair_status_state"`
}

func loadContract(t *testing.T) statusContract {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "desktop-status-contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c statusContract
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func checkType(t *testing.T, field string, spec, v any) {
	t.Helper()
	switch spec := spec.(type) {
	case string:
		ok := false
		switch spec {
		case "bool":
			_, ok = v.(bool)
		case "string":
			_, ok = v.(string)
		case "int":
			f, isNum := v.(float64)
			ok = isNum && f == float64(int64(f))
		case "object":
			_, ok = v.(map[string]any)
		}
		if !ok {
			t.Errorf("status.%s = %#v, contract says %s", field, v, spec)
		}
	case []any:
		for _, a := range spec {
			if a == v {
				return
			}
		}
		t.Errorf("status.%s = %#v, contract allows %v", field, v, spec)
	}
}

// checkStatus validates a "status" response exactly as Dart receives it.
func checkStatus(t *testing.T, c statusContract, st map[string]any) {
	t.Helper()
	for f, spec := range c.Status {
		v, ok := st[f]
		if !ok {
			t.Errorf("status is missing %q (read by the Dart UI)", f)
			continue
		}
		checkType(t, f, spec, v)
	}
	for f, spec := range c.Optional {
		if v, ok := st[f]; ok {
			checkType(t, f, spec, v)
		}
	}
	if st["connected"] != st["gateway_connected"] {
		t.Errorf("connected (%v) must be this device's gateway session (%v)", st["connected"], st["gateway_connected"])
	}
}

func call(t *testing.T, req map[string]any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(req)
	var out struct {
		OK    bool           `json:"ok"`
		Data  map[string]any `json:"data"`
		Error string         `json:"error"`
	}
	if err := json.Unmarshal([]byte(callJSON(string(b))), &out); err != nil || !out.OK {
		t.Fatalf("%v: %s %v", req["action"], out.Error, err)
	}
	return out.Data
}

// Drives the kernel with the exact JSON desktop/lib/native_api.dart sends:
// start, status, pair_prepare(link), pair_approve, pair_status, status.
func TestStatusContractWithDart(t *testing.T) {
	c := loadContract(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, _ := store.OpenSQLite(":memory:", nil)
	defer st.Close()
	gw, _ := gateway.New(gateway.Config{Store: st, Logger: log.New(io.Discard, "", 0)})
	key := strings.Repeat("k", 40)
	// the homepage does not serve /.well-known/localsubproxy (like the mock
	// tenant https://acme.example); it must not affect "connected"
	_ = gw.RegisterTenantProfile(ctx, "acme", key, store.TenantProfile{DisplayName: "Acme Chat (mock)", HomepageURI: "https://acme.example",
		PairingURI: "http://127.0.0.1/link", RequestedScopes: []string{lmbp.ScopeChat}})
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	var started struct{ OK bool }
	if err := json.Unmarshal([]byte(startJSON(`{"data_dir":"`+t.TempDir()+`"}`)), &started); err != nil || !started.OK {
		t.Fatal("start failed")
	}
	defer callJSON(`{"action":"stop"}`)
	s := call(t, map[string]any{"action": "status"})
	checkStatus(t, c, s)
	if s["paired"] != false || s["connected"] != false || s["pairing_state"] != "idle" {
		t.Fatalf("fresh status %v", s)
	}

	call(t, map[string]any{"action": "pair_prepare", "link": lmbp.PairLink{Gateway: srv.URL, Tenant: "acme"}.String()})
	checkStatus(t, c, call(t, map[string]any{"action": "status"}))
	code, _ := call(t, map[string]any{"action": "pair_approve"})["user_code"].(string)
	b, _ := json.Marshal(map[string]string{"user_code": code})
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/lmbp/v1/tenant/devices/approvals", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-End-User", "alice")
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != 200 {
		t.Fatalf("tenant approve: %v", err)
	}
	var ps map[string]any
	for i := 0; i < 100; i++ {
		ps = call(t, map[string]any{"action": "pair_status"})
		if ps["state"] == "paired" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	allowed := false
	for _, a := range c.PairStatusState {
		allowed = allowed || ps["state"] == a
	}
	if !allowed || ps["state"] != "paired" {
		t.Fatalf("pair_status %v", ps)
	}
	for i := 0; i < 100; i++ {
		if s = call(t, map[string]any{"action": "status"}); s["connected"] == true {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // give the legacy remote probe a chance to run
	s = call(t, map[string]any{"action": "status"})
	checkStatus(t, c, s)
	if s["paired"] != true || s["connected"] != true || s["transport"] != "ws" || s["saas_name"] != "Acme Chat (mock)" {
		t.Fatalf("paired+online status %v", s)
	}
}

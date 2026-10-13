package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"localsubproxy/internal/lsp2"
	"localsubproxy/internal/lsp2/store"
)

func setup(t *testing.T) (*Gateway, *httptest.Server) {
	st, err := store.OpenSQLite(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	g, _ := New(Config{Store: st, Logger: log.New(io.Discard, "", 0)})
	_ = g.RegisterTenant(context.Background(), "acme", "Acme", strings.Repeat("a", 40))
	_ = g.RegisterTenant(context.Background(), "beta", "Beta", strings.Repeat("b", 40))
	s := httptest.NewServer(g.Handler())
	t.Cleanup(func() { s.Close(); st.Close() })
	return g, s
}

func post(t *testing.T, url, key, user string, body any) (int, map[string]any) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-End-User", user)
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(r.Body).Decode(&m)
	return r.StatusCode, m
}

func TestDeviceFlow(t *testing.T) {
	_, s := setup(t)
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	_, a := post(t, s.URL+"/v2/device/authorize", "", "", map[string]string{"tenant": "acme", "public_key": lsp2.B64(pub), "device_name": "pc"})
	dc, uc := a["device_code"].(string), a["user_code"].(string)
	proof := func() string { return lsp2.SignProof(key, "", lsp2.ProofToken, dc, time.Now()) }
	tok := func(p string) (int, map[string]any) {
		return post(t, s.URL+"/v2/token", "", "", map[string]string{"grant_type": deviceGrant, "device_code": dc, "proof": p})
	}
	if _, m := tok(proof()); m["error"] != "authorization_pending" && m["error"] != "slow_down" {
		t.Fatalf("expected pending, got %v", m)
	}
	replayed := proof()
	_, _ = tok(replayed)
	if _, m := tok(replayed); m["error"] != "invalid_grant" {
		t.Fatalf("replayed proof accepted: %v", m)
	}
	if _, m := tok(proof()); m["error"] != "slow_down" {
		t.Fatalf("expected slow_down, got %v", m)
	}
	// Another tenant cannot approve this code.
	if st, _ := post(t, s.URL+"/v2/tenant/device-approvals", strings.Repeat("b", 40), "mallory", map[string]string{"user_code": uc}); st != 404 {
		t.Fatalf("cross-tenant approval status %d", st)
	}
	if st, _ := post(t, s.URL+"/v2/tenant/device-approvals", "wrong-key", "alice", map[string]string{"user_code": uc}); st != 401 {
		t.Fatal("bad tenant key accepted")
	}
	if st, m := post(t, s.URL+"/v2/tenant/device-approvals", strings.Repeat("a", 40), "alice", map[string]string{"user_code": strings.ToLower(strings.ReplaceAll(uc, "-", ""))}); st != 200 || m["status"] != "approved" {
		t.Fatalf("approve %d %v", st, m)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if _, m := tok(lsp2.SignProof(other, "", lsp2.ProofToken, dc, time.Now())); m["error"] != "invalid_grant" {
		t.Fatal("token issued without device-key proof")
	}
	time.Sleep(2 * time.Second)
	st, m := tok(proof())
	if st != 200 || m["access_token"] == nil {
		t.Fatalf("token %d %v", st, m)
	}
	if _, m := tok(proof()); m["error"] != "invalid_grant" {
		t.Fatal("device code reusable")
	}
	// Key-bound refresh via device-signed assertion; replayed assertion rejected.
	now := time.Now()
	devID := m["device_id"].(string)
	as := lsp2.SignJWT(key, lsp2.TypAssertion, "d", lsp2.Claims{Iss: devID, Sub: devID, Aud: Audience + "/token", Iat: now.Unix(), Exp: now.Add(time.Minute).Unix(), Jti: "x1"})
	if st, _ := post(t, s.URL+"/v2/token", "", "", map[string]string{"grant_type": jwtBearerGrant, "assertion": as}); st != 200 {
		t.Fatal("refresh failed")
	}
	if _, m := post(t, s.URL+"/v2/token", "", "", map[string]string{"grant_type": jwtBearerGrant, "assertion": as}); m["error"] != "invalid_grant" {
		t.Fatal("assertion replay accepted")
	}
	// WebSocket rejects tokens for missing subprotocol / no token.
	r, _ := http.Get(s.URL + "/v2/connect")
	if r.StatusCode != 400 {
		t.Fatalf("connect without subprotocol: %d", r.StatusCode)
	}
}

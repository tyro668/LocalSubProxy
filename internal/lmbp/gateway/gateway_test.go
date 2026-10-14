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

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/store"
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
	_, a := post(t, s.URL+"/lmbp/v1/device/authorize", "", "", map[string]string{"tenant": "acme", "public_key": lmbp.B64(pub), "device_name": "pc"})
	dc, uc := a["device_code"].(string), a["user_code"].(string)
	proof := func() string { return lmbp.SignProof(key, "", lmbp.ProofToken, dc, time.Now()) }
	tok := func(p string) (int, map[string]any) {
		return post(t, s.URL+"/lmbp/v1/token", "", "", map[string]string{"grant_type": deviceGrant, "device_code": dc, "proof": p})
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
	if st, _ := post(t, s.URL+"/lmbp/v1/tenant/devices/approvals", strings.Repeat("b", 40), "mallory", map[string]string{"user_code": uc}); st != 404 {
		t.Fatalf("cross-tenant approval status %d", st)
	}
	if st, _ := post(t, s.URL+"/lmbp/v1/tenant/devices/approvals", "wrong-key", "alice", map[string]string{"user_code": uc}); st != 401 {
		t.Fatal("bad tenant key accepted")
	}
	if st, m := post(t, s.URL+"/lmbp/v1/tenant/devices/approvals", strings.Repeat("a", 40), "alice", map[string]string{"user_code": strings.ToLower(strings.ReplaceAll(uc, "-", ""))}); st != 200 || m["status"] != "approved" {
		t.Fatalf("approve %d %v", st, m)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if _, m := tok(lmbp.SignProof(other, "", lmbp.ProofToken, dc, time.Now())); m["error"] != "invalid_grant" {
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
	at, _ := lmbp.UnverifiedClaims(m["access_token"].(string))
	if at.Iss != s.URL || at.Aud != lmbp.TokenAudience(s.URL, "acme") || m["issuer"] != s.URL {
		t.Fatalf("token not bound to issuer/tenant: iss=%q aud=%q", at.Iss, at.Aud)
	}
	wrongAud := lmbp.SignJWT(key, lmbp.TypAssertion, lmbp.Thumbprint(key.Public().(ed25519.PublicKey)), lmbp.Claims{Iss: devID, Sub: devID, Aud: "https://other.example/lmbp/v1/token", Iat: now.Unix(), Exp: now.Add(time.Minute).Unix(), Jti: "x0"})
	if _, m := post(t, s.URL+"/lmbp/v1/token", "", "", map[string]string{"grant_type": jwtBearerGrant, "assertion": wrongAud}); m["error"] != "invalid_grant" {
		t.Fatal("assertion for another gateway accepted")
	}
	as := lmbp.SignJWT(key, lmbp.TypAssertion, lmbp.Thumbprint(key.Public().(ed25519.PublicKey)), lmbp.Claims{Iss: devID, Sub: devID, Aud: lmbp.AssertionAudience(s.URL), Iat: now.Unix(), Exp: now.Add(time.Minute).Unix(), Jti: "x1"})
	if st, _ := post(t, s.URL+"/lmbp/v1/token", "", "", map[string]string{"grant_type": jwtBearerGrant, "assertion": as}); st != 200 {
		t.Fatal("refresh failed")
	}
	if _, m := post(t, s.URL+"/lmbp/v1/token", "", "", map[string]string{"grant_type": jwtBearerGrant, "assertion": as}); m["error"] != "invalid_grant" {
		t.Fatal("assertion replay accepted")
	}
	// WebSocket rejects tokens for missing subprotocol / no token.
	r, _ := http.Get(s.URL + "/lmbp/v1/connect")
	if r.StatusCode != 400 {
		t.Fatalf("connect without subprotocol: %d", r.StatusCode)
	}
}

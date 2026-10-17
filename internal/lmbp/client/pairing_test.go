package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/gateway"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/store"
)

const pairKey = "tk_pppppppppppppppppppppppppppppppppppppppp"

func pairRig(t *testing.T) (*gateway.Gateway, *httptest.Server) {
	st, _ := store.OpenSQLite(":memory:", nil)
	g, _ := gateway.New(gateway.Config{Store: st, Logger: log.New(io.Discard, "", 0)})
	_ = g.RegisterTenantProfile(context.Background(), "acme", pairKey, store.TenantProfile{DisplayName: "Acme CRM", LogoURI: "https://acme.example/logo.png",
		HomepageURI: "https://acme.example", PolicyURI: "https://acme.example/privacy", PairingURI: "http://127.0.0.1/link", RequestedModels: []string{"qwen3"}, RequestedScopes: []string{lmbp.ScopeChat}})
	s := httptest.NewServer(g.Handler())
	t.Cleanup(func() { s.Close(); st.Close() })
	return g, s
}

func approve(t *testing.T, s *httptest.Server, code string, deny bool) {
	b, _ := json.Marshal(map[string]any{"user_code": code, "deny": deny})
	req, _ := http.NewRequest("POST", s.URL+"/lmbp/v1/tenant/devices/approvals", strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+pairKey)
	req.Header.Set("X-End-User", "alice")
	r, err := http.DefaultClient.Do(req)
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("approve: %v %v", err, r)
	}
	r.Body.Close()
}

func TestPairLinkConsentApprove(t *testing.T) {
	g, s := pairRig(t)
	ctx := context.Background()
	link := lmbp.PairLink{Gateway: s.URL, Tenant: "acme"}.String()
	req, err := PrepareLink(ctx, nil, link)
	if err != nil {
		t.Fatal(err)
	}
	c := req.Consent
	if !c.Verified || c.SaaS.Name != "Acme CRM" || c.SaaS.PolicyURI != "https://acme.example/privacy" || c.Gateway != s.URL || c.GatewayKeyID != lmbp.Thumbprint(g.PublicKey()) ||
		len(c.RequestedModels) != 1 || c.RequestedScopes[0] != lmbp.ScopeChat {
		t.Fatalf("consent %+v", c)
	}
	p, err := req.Approve(ctx, "pc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.LinkURL, "http://127.0.0.1/link?user_code=") {
		t.Fatalf("link %s", p.LinkURL)
	}
	if _, err := req.Approve(ctx, "pc"); err == nil {
		t.Fatal("request approved twice")
	}
	approve(t, s, p.UserCode, false)
	id, err := p.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if id.Issuer != s.URL || id.Audience != lmbp.TokenAudience(s.URL, "acme") || id.SaaSName != "Acme CRM" {
		t.Fatalf("identity %+v", id)
	}
	cl, err := New(Config{Executor: &OpenAIExecutor{ModelList: []string{"m"}}, Logger: log.New(io.Discard, "", 0)}, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cl.accessToken(ctx); err != nil {
		t.Fatalf("token refresh: %v", err)
	}
	// The same gateway reached under another name (different issuer) is rejected.
	bad := id
	bad.Issuer = strings.Replace(s.URL, "127.0.0.1", "localhost", 1)
	bad.Audience = lmbp.TokenAudience(bad.Issuer, "acme")
	cl2, _ := New(Config{Executor: &OpenAIExecutor{ModelList: []string{"m"}}, Logger: log.New(io.Discard, "", 0)}, bad)
	if _, _, err := cl2.accessToken(ctx); err == nil {
		t.Fatal("token from a mismatched issuer accepted")
	}
}

func TestConsentDeniedLocallyAndRemotely(t *testing.T) {
	_, s := pairRig(t)
	ctx := context.Background()
	req, err := PrepareLink(ctx, nil, lmbp.PairLink{Gateway: s.URL, Tenant: "acme"}.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := req.Deny(); !IsConsentDenied(err) {
		t.Fatalf("deny: %v", err)
	}
	if _, err := req.Approve(ctx, "pc"); err == nil {
		t.Fatal("approved after deny")
	}
	req, _ = PrepareLink(ctx, nil, lmbp.PairLink{Gateway: s.URL, Tenant: "acme"}.String())
	p, err := req.Approve(ctx, "pc")
	if err != nil {
		t.Fatal(err)
	}
	approve(t, s, p.UserCode, true)
	if _, err := p.Wait(ctx); !IsConsentDenied(err) {
		t.Fatalf("remote deny: %v", err)
	}
	var ce *ConsentError
	if errors.As(err, &ce); ce != nil && ce.Code() != lmbp.ErrConsentDenied {
		t.Fatal("wrong code")
	}
}

// Metadata signed by a key that is not in the gateway JWKS, or claiming
// another issuer, must not produce a consent screen.
func TestRejectsForgedMetadata(t *testing.T) {
	_, rogue, _ := ed25519.GenerateKey(rand.Reader)
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		_, other, _ := ed25519.GenerateKey(rand.Reader)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []lmbp.JWK{lmbp.PublicJWK(other.Public().(ed25519.PublicKey))}})
	})
	mux.HandleFunc(lmbp.MetadataPath, func(w http.ResponseWriter, r *http.Request) {
		iss := srv.URL
		m := lmbp.GatewayMetadata{Issuer: iss, JWKSURI: iss + "/.well-known/jwks.json", DeviceAuthorizationEndpoint: iss + "/lmbp/v1/device/authorize", TokenEndpoint: iss + "/lmbp/v1/token",
			Tenant: "acme", Audience: lmbp.TokenAudience(iss, "acme"), SaaS: &lmbp.SaaSInfo{Name: "Totally Legit Bank"}, Iat: time.Now().Unix(), Exp: time.Now().Add(time.Hour).Unix()}
		m.SignedMetadata = lmbp.SignMetadata(rogue, "", m)
		_ = json.NewEncoder(w).Encode(m)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	if _, err := PrepareLink(context.Background(), nil, lmbp.PairLink{Gateway: srv.URL, Tenant: "acme"}.String()); err == nil {
		t.Fatal("metadata signed by a non-JWKS key accepted")
	}
}

func TestParsePairLink(t *testing.T) {
	ok := []string{"lmbp://pair?v=2&gateway=https%3A%2F%2Fgw.example.com&tenant=acme", "lmbp://pair?v=2&gateway=https://GW.example.com:443&tenant=a.b-c&remote=https://crm.example"}
	for _, s := range ok {
		l, err := lmbp.ParsePairLink(s)
		if err != nil || l.Gateway != "https://gw.example.com" {
			t.Fatalf("%s: %v %+v", s, err, l)
		}
	}
	bad := []string{"https://pair?v=1&gateway=https://gw&tenant=a", "lmbp://pair?gateway=https://gw.example.com&tenant=a", "lmbp://pair?v=2&gateway=http://gw.example.com&tenant=a",
		"lmbp://pair?v=2&gateway=https://gw.example.com/path&tenant=a", "lmbp://pair?v=2&gateway=https://gw.example.com&tenant=a/b", "lmbp://pair?v=1&v=2&gateway=https://gw.example.com&tenant=a", "lmbp://pair?v=1&gateway=https://gw.example.com&tenant=a",
		"lmbp://pair?v=2&gateway=https://u:p@gw.example.com&tenant=a"}
	for _, s := range bad {
		if _, err := lmbp.ParsePairLink(s); err == nil {
			t.Fatalf("accepted %s", s)
		}
	}
}

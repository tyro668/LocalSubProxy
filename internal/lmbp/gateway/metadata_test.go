package gateway

import (
	"context"
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

func TestMetadataSignedAndBound(t *testing.T) {
	st, _ := store.OpenSQLite(":memory:", nil)
	defer st.Close()
	g, err := New(Config{Store: st, Issuer: "https://GW.example.com:443/", Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	if g.Issuer() != "https://gw.example.com" {
		t.Fatalf("issuer not normalized: %q", g.Issuer())
	}
	if _, err := New(Config{Store: st, Issuer: "http://gw.example.com"}); err == nil {
		t.Fatal("non-loopback http issuer accepted")
	}
	prof := store.TenantProfile{DisplayName: "Acme CRM", LogoURI: "https://acme.example/logo.png", HomepageURI: "https://acme.example", PolicyURI: "https://acme.example/privacy",
		RequestedModels: []string{"qwen3"}, RequestedScopes: []string{lmbp.ScopeChat, lmbp.ScopeTools}}
	if err := g.RegisterTenantProfile(context.Background(), "acme", strings.Repeat("a", 40), prof); err != nil {
		t.Fatal(err)
	}
	if err := g.RegisterTenantProfile(context.Background(), "x", strings.Repeat("a", 41), store.TenantProfile{RequestedScopes: []string{"root"}}); err == nil {
		t.Fatal("unknown scope accepted")
	}
	s := httptest.NewServer(g.Handler())
	defer s.Close()
	get := func(q string) (int, lmbp.GatewayMetadata) {
		r, err := http.Get(s.URL + lmbp.MetadataPath + q)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var m lmbp.GatewayMetadata
		_ = json.NewDecoder(r.Body).Decode(&m)
		return r.StatusCode, m
	}
	if code, m := get(""); code != 200 || m.Issuer != "https://gw.example.com" || m.SignedMetadata != "" || m.ConnectEndpoint != "wss://gw.example.com/lmbp/v1/connect" {
		t.Fatalf("base metadata %d %+v", code, m)
	}
	if code, _ := get("?tenant=nobody"); code != 404 {
		t.Fatalf("unknown tenant: %d", code)
	}
	_, m := get("?tenant=acme")
	sm, err := lmbp.VerifyMetadata(g.PublicKey(), m.SignedMetadata, "https://gw.example.com", "acme", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if sm.SaaS.Name != "Acme CRM" || sm.SaaS.PolicyURI != prof.PolicyURI || sm.Audience != "https://gw.example.com/tenants/acme" || len(sm.RequestedScopes) != 2 {
		t.Fatalf("signed metadata %+v", sm)
	}
	if _, err := lmbp.VerifyMetadata(g.PublicKey(), m.SignedMetadata, "https://evil.example", "acme", time.Now()); err == nil {
		t.Fatal("metadata accepted for another issuer")
	}
	if _, err := lmbp.VerifyMetadata(g.PublicKey(), m.SignedMetadata, "https://gw.example.com", "beta", time.Now()); err == nil {
		t.Fatal("metadata accepted for another tenant")
	}
	if _, err := lmbp.VerifyMetadata(g.PublicKey(), m.SignedMetadata, "https://gw.example.com", "acme", time.Now().Add(2*time.Hour)); err == nil {
		t.Fatal("expired metadata accepted")
	}
	// A token minted for this tenant at this issuer is rejected for another issuer.
	d := store.Device{ID: "dev_1", TenantID: "acme", PublicKey: g.PublicKey()}
	tok := g.issue(d, g.Issuer())["access_token"].(string)
	if _, err := g.verifyAccess(tok, g.Issuer()); err != nil {
		t.Fatal(err)
	}
	if _, err := g.verifyAccess(tok, "https://other.example"); err == nil {
		t.Fatal("token accepted by another issuer")
	}
}

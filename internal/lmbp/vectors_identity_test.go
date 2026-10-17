package lmbp

import (
	"crypto/ed25519"
	"encoding/json"
	"testing"
	"time"
)

// §18–§19 vectors: issuer/audience binding, signed gateway metadata and the
// lmbp://pair deep link.

type vecMetaNeg struct {
	Name    string `json:"name"`
	Compact string `json:"compact"`
	Issuer  string `json:"expected_issuer"`
	Tenant  string `json:"expected_tenant"`
	Why     string `json:"must_reject_because"`
}

type vecLink struct {
	Link    string `json:"link"`
	Gateway string `json:"gateway,omitempty"`
	Tenant  string `json:"tenant,omitempty"`
	Remote  string `json:"remote,omitempty"`
	Why     string `json:"must_reject_because,omitempty"`
}

type vecIdentity struct {
	Description       string       `json:"description"`
	Issuer            string       `json:"issuer"`
	Tenant            string       `json:"tenant"`
	TokenAudience     string       `json:"token_audience"`
	AssertionAudience string       `json:"assertion_audience"`
	OriginCases       [][2]string  `json:"origin_normalization"`
	BadOrigins        []string     `json:"invalid_origins"`
	SignedMetadata    vecJWS       `json:"signed_metadata"`
	BadMetadata       []vecMetaNeg `json:"invalid_signed_metadata"`
	PairLinks         []vecLink    `json:"pair_links"`
	BadPairLinks      []vecLink    `json:"invalid_pair_links"`
	ForeignToken      string       `json:"access_token_other_issuer"`
}

func genIdentity(gk ed25519.PrivateKey, kid string, at Claims) *vecIdentity {
	m := GatewayMetadata{Issuer: vecIssuer, JWKSURI: vecIssuer + "/.well-known/jwks.json", DeviceAuthorizationEndpoint: vecIssuer + "/lmbp/v1/device/authorize",
		TokenEndpoint: vecIssuer + "/lmbp/v1/token", ConnectEndpoint: "wss://gw.example.com/lmbp/v1/connect", ProtocolVersions: ProtocolVersions, Profiles: Profiles, Capabilities: Capabilities, ScopesSupported: Scopes,
		Tenant: "acme", Audience: TokenAudience(vecIssuer, "acme"), SaaS: &SaaSInfo{Name: "Acme CRM", LogoURI: "https://acme.example/logo.png", HomepageURI: "https://acme.example",
			PolicyURI: "https://acme.example/privacy", PairingURI: "https://acme.example/lsp/link"}, RequestedModels: []string{"lm/qwen3-8b"}, RequestedScopes: []string{ScopeChat, ScopeTools},
		Iat: vecIat, Exp: vecIat + 3600}
	sm := mkJWS("signed_metadata", "metadata", "gateway", gk, TypMetadata, kid, m, map[string]any{"issuer": vecIssuer, "tenant": "acme"})
	other := m
	other.Issuer = "https://evil.example"
	long := m
	long.Exp = vecIat + 48*3600
	offOrigin := m
	offOrigin.JWKSURI = "https://cdn.example/jwks.json"
	dkey, _ := vkey("lmbp test vector: device key")
	foreign := at
	foreign.Iss = "https://other-gateway.example"
	foreign.Aud = TokenAudience(foreign.Iss, "acme")
	return &vecIdentity{
		Description:       "§18/§19. Verify signed_metadata with keys.gateway at time 'now' bound to issuer+tenant; reject every invalid_signed_metadata entry. access_token_other_issuer is validly signed by the gateway key but MUST be rejected by a device whose identity has issuer=" + vecIssuer + ".",
		Issuer:            vecIssuer,
		Tenant:            "acme",
		TokenAudience:     TokenAudience(vecIssuer, "acme"),
		AssertionAudience: AssertionAudience(vecIssuer),
		OriginCases:       [][2]string{{"https://GW.Example.com:443/", vecIssuer}, {"https://gw.example.com:8443", "https://gw.example.com:8443"}, {"http://127.0.0.1:8787", "http://127.0.0.1:8787"}},
		BadOrigins:        []string{"http://gw.example.com", "https://gw.example.com/path", "https://u:p@gw.example.com", "https://gw.example.com?x=1", "ftp://gw.example.com"},
		SignedMetadata:    sm,
		BadMetadata: []vecMetaNeg{
			{"wrong_expected_issuer", sm.Compact, "https://evil.example", "acme", "metadata issuer differs from the gateway the device was pointed at"},
			{"wrong_expected_tenant", sm.Compact, vecIssuer, "beta", "metadata is for another tenant"},
			{"signed_by_device_key", mkJWS("", "", "", dkey, TypMetadata, "", m, nil).Compact, vecIssuer, "acme", "not signed by a gateway JWKS key"},
			{"issuer_claim_mismatch", mkJWS("", "", "", gk, TypMetadata, kid, other, nil).Compact, vecIssuer, "acme", "signed issuer is another origin"},
			{"validity_over_24h", mkJWS("", "", "", gk, TypMetadata, kid, long, nil).Compact, vecIssuer, "acme", "exp - iat > 24h"},
			{"jwks_off_origin", mkJWS("", "", "", gk, TypMetadata, kid, offOrigin, nil).Compact, vecIssuer, "acme", "jwks_uri not on the issuer origin"},
			{"wrong_typ", mkJWS("", "", "", gk, TypAccessToken, kid, m, nil).Compact, vecIssuer, "acme", "typ must be lmbp-metadata+jwt"},
		},
		PairLinks: []vecLink{
			{Link: "lmbp://pair?v=2&gateway=https%3A%2F%2Fgw.example.com&tenant=acme", Gateway: vecIssuer, Tenant: "acme"},
			{Link: "lmbp://pair?v=2&gateway=https://GW.example.com:443&tenant=acme&remote=https://acme.example", Gateway: vecIssuer, Tenant: "acme", Remote: "https://acme.example"},
		},
		BadPairLinks: []vecLink{
			{Link: "lmbp://pair?gateway=https://gw.example.com&tenant=acme", Why: "missing v=2"},
			{Link: "lmbp://pair?v=2&gateway=http://gw.example.com&tenant=acme", Why: "gateway must be https (non-loopback)"},
			{Link: "lmbp://pair?v=2&gateway=https://gw.example.com/x&tenant=acme", Why: "gateway must be an origin"},
			{Link: "lmbp://pair?v=2&gateway=https://gw.example.com&tenant=a/b", Why: "tenant pattern"},
			{Link: "lmbp://pair?v=2&gateway=https://gw.example.com&tenant=acme&tenant=beta", Why: "duplicate parameter"},
			{Link: "lmbp://pair?v=1&gateway=https://gw.example.com&tenant=acme", Why: "v=1 is an LMBP 1.x link (2.0 requires v=2)"},
			{Link: "https://pair?v=1&gateway=https://gw.example.com&tenant=acme", Why: "scheme must be lmbp"},
		},
		ForeignToken: mkJWS("", "", "", gk, TypAccessToken, kid, foreign, nil).Compact,
	}
}

func checkIdentity(t *testing.T, v vectors) {
	id := v.Identity
	if id == nil {
		t.Fatal("identity vectors missing")
	}
	now := time.Unix(v.Now, 0)
	gpub, _ := UnB64(v.Keys["gateway"].Public)
	if TokenAudience(id.Issuer, id.Tenant) != id.TokenAudience || AssertionAudience(id.Issuer) != id.AssertionAudience {
		t.Fatal("audience derivation")
	}
	for _, c := range id.OriginCases {
		if o, err := Origin(c[0]); err != nil || o != c[1] {
			t.Fatalf("origin %q -> %q %v", c[0], o, err)
		}
	}
	for _, b := range id.BadOrigins {
		if _, err := Origin(b); err == nil {
			t.Fatalf("origin %q accepted", b)
		}
	}
	m, err := VerifyMetadata(gpub, id.SignedMetadata.Compact, id.Issuer, id.Tenant, now)
	if err != nil || m.SaaS.Name != "Acme CRM" {
		t.Fatalf("signed metadata: %v", err)
	}
	for _, n := range id.BadMetadata {
		if _, err := VerifyMetadata(gpub, n.Compact, n.Issuer, n.Tenant, now); err == nil {
			t.Fatalf("bad metadata %s accepted", n.Name)
		}
	}
	for _, l := range id.PairLinks {
		p, err := ParsePairLink(l.Link)
		if err != nil || p.Gateway != l.Gateway || p.Tenant != l.Tenant || p.Remote != l.Remote {
			t.Fatalf("pair link %s: %v %+v", l.Link, err, p)
		}
	}
	for _, l := range id.BadPairLinks {
		if _, err := ParsePairLink(l.Link); err == nil {
			t.Fatalf("bad pair link accepted: %s", l.Link)
		}
	}
	c, err := VerifyJWT(gpub, id.ForeignToken, TypAccessToken, id.TokenAudience, now)
	if err == nil && c.Iss == id.Issuer {
		t.Fatal("foreign-issuer token accepted")
	}
	b, _ := json.Marshal(id)
	if len(b) == 0 {
		t.Fatal()
	}
}

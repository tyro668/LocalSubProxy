package gateway

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lsp2"
	"github.com/tyro668/LocalSubProxy/internal/lsp2/store"
)

// metadataTTL is the validity of signed_metadata.
const metadataTTL = time.Hour

// Issuer returns the configured issuer origin ("" in development mode).
func (g *Gateway) Issuer() string { return g.cfg.Issuer }

// issuer is the configured origin, or (development mode only) the origin the
// request was addressed to.
func (g *Gateway) issuer(r *http.Request) string {
	if g.cfg.Issuer != "" {
		return g.cfg.Issuer
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if o, err := lsp2.Origin(scheme + "://" + r.Host); err == nil {
		return o
	}
	return scheme + "://" + strings.ToLower(r.Host)
}

// verifyAccess checks an access token: signature by the gateway key, typ,
// iss == this gateway, aud == TokenAudience(iss, tenant) and lifetime.
func (g *Gateway) verifyAccess(tok, iss string) (lsp2.Claims, error) {
	uc, err := lsp2.UnverifiedClaims(tok)
	if err != nil {
		return uc, err
	}
	c, err := lsp2.VerifyJWT(g.pub, tok, lsp2.TypAccessToken, lsp2.TokenAudience(iss, uc.Tenant), time.Now())
	if err != nil {
		return c, err
	}
	if c.Iss != iss {
		return c, errors.New("token issuer mismatch")
	}
	return c, nil
}

// Metadata builds the gateway metadata document; with a tenant it adds the
// SaaS identity block and the gateway-signed signed_metadata JWS.
func (g *Gateway) Metadata(iss string, t *store.Tenant) lsp2.GatewayMetadata {
	ws := "ws" + strings.TrimPrefix(iss, "http")
	m := lsp2.GatewayMetadata{Issuer: iss, JWKSURI: iss + "/.well-known/jwks.json", DeviceAuthorizationEndpoint: iss + "/v2/device/authorize",
		TokenEndpoint: iss + "/v2/token", ConnectEndpoint: ws + "/v2/connect", ChatCompletionsEndpoint: iss + "/v1/chat/completions",
		ProtocolVersions: []string{"2"}, Capabilities: lsp2.Capabilities, ScopesSupported: lsp2.Scopes}
	if t == nil {
		return m
	}
	p := t.Profile
	name := p.DisplayName
	if name == "" {
		name = t.Name
	}
	scopes := p.RequestedScopes
	if len(scopes) == 0 {
		scopes = []string{lsp2.ScopeChat}
	}
	now := time.Now()
	m.Tenant, m.Audience = t.ID, lsp2.TokenAudience(iss, t.ID)
	m.SaaS = &lsp2.SaaSInfo{Name: name, LogoURI: p.LogoURI, HomepageURI: p.HomepageURI, PolicyURI: p.PolicyURI, TosURI: p.TosURI, PairingURI: p.PairingURI}
	m.RequestedModels, m.RequestedScopes = p.RequestedModels, scopes
	m.Iat, m.Exp = now.Unix(), now.Add(metadataTTL).Unix()
	m.SignedMetadata = lsp2.SignMetadata(g.cfg.SigningKey, g.kid, m)
	return m
}

func (g *Gateway) metadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		problem(w, 405, lsp2.ErrBadRequest, "GET required")
		return
	}
	var t *store.Tenant
	if id := r.URL.Query().Get("tenant"); id != "" {
		tt, err := g.cfg.Store.GetTenant(r.Context(), id)
		if err != nil || !lsp2.TenantIDPattern.MatchString(id) {
			problem(w, 404, lsp2.ErrBadRequest, "unknown tenant")
			return
		}
		t = &tt
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	m := g.Metadata(g.issuer(r), t)
	w.Header().Set("Content-Type", "application/json")
	writeJSONCache(w, m)
}

package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"localsubproxy/internal/lsp2"
)

// ConsentError is returned when pairing ends without the user's consent:
// the local user denied the SaaS on the consent screen, or the signed-in user
// denied the request on the SaaS. Code is always lsp2.ErrConsentDenied.
type ConsentError struct{ Reason string }

func (e *ConsentError) Error() string { return "CONSENT_DENIED: " + e.Reason }
func (e *ConsentError) Code() string  { return lsp2.ErrConsentDenied }

// IsConsentDenied reports whether err is a ConsentError.
func IsConsentDenied(err error) bool { var c *ConsentError; return errors.As(err, &c) }

// Consent is everything the desktop must show before the user approves a
// pairing. All of it comes from the gateway's signed metadata (§19), never
// from the deep link or the discovery document.
type Consent struct {
	SaaS            lsp2.SaaSInfo `json:"saas"`
	Gateway         string        `json:"gateway"` // issuer origin
	Tenant          string        `json:"tenant"`
	Audience        string        `json:"audience"`
	RequestedModels []string      `json:"requested_models"`
	RequestedScopes []string      `json:"requested_scopes"`
	GatewayKeyID    string        `json:"gateway_key_id"` // RFC 7638 thumbprint of the metadata signing key
	Remote          string        `json:"remote,omitempty"`
	Verified        bool          `json:"verified"` // signed_metadata verified against the gateway JWKS
}

// PairRequest is a verified, not yet consented pairing. Call Approve after
// the user explicitly confirmed Consent, or Deny.
type PairRequest struct {
	Consent   Consent
	hc        *http.Client
	meta      lsp2.GatewayMetadata
	gwKey     ed25519.PublicKey
	remoteURL string
	linkURL   string
	decided   bool
}

// Prepare discovers the gateway from a SaaS origin (/.well-known/lsp-connector)
// and verifies the gateway's signed metadata.
func Prepare(ctx context.Context, hc *http.Client, remoteURL string) (*PairRequest, error) {
	if hc == nil {
		hc = defaultHTTP()
	}
	disc, err := Discover(ctx, hc, remoteURL)
	if err != nil {
		return nil, err
	}
	return prepare(ctx, hc, disc.GatewayURL, disc.Tenant, remoteURL, disc.LinkURL)
}

// PrepareLink starts pairing from an lsp2://pair deep link.
func PrepareLink(ctx context.Context, hc *http.Client, link string) (*PairRequest, error) {
	if hc == nil {
		hc = defaultHTTP()
	}
	l, err := lsp2.ParsePairLink(link)
	if err != nil {
		return nil, err
	}
	return prepare(ctx, hc, l.Gateway, l.Tenant, l.Remote, "")
}

func getJSON(ctx context.Context, hc *http.Client, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(out)
}

func prepare(ctx context.Context, hc *http.Client, gatewayURL, tenant, remote, linkURL string) (*PairRequest, error) {
	iss, err := lsp2.Origin(gatewayURL)
	if err != nil {
		return nil, fmt.Errorf("gateway URL: %w", err)
	}
	if !lsp2.TenantIDPattern.MatchString(tenant) {
		return nil, errors.New("invalid tenant id")
	}
	var meta lsp2.GatewayMetadata
	if err := getJSON(ctx, hc, iss+lsp2.MetadataPath+"?tenant="+url.QueryEscape(tenant), &meta); err != nil {
		return nil, fmt.Errorf("gateway metadata: %w", err)
	}
	if meta.Issuer != iss {
		return nil, fmt.Errorf("gateway metadata issuer %q does not match %q", meta.Issuer, iss)
	}
	if meta.SignedMetadata == "" {
		return nil, errors.New("gateway metadata is not signed")
	}
	if !lsp2.SameOrigin(meta.JWKSURI, iss) {
		return nil, errors.New("jwks_uri is not on the gateway origin")
	}
	var jwks struct {
		Keys []lsp2.JWK `json:"keys"`
	}
	if err := getJSON(ctx, hc, meta.JWKSURI, &jwks); err != nil {
		return nil, fmt.Errorf("gateway JWKS: %w", err)
	}
	kid := jwsKid(meta.SignedMetadata)
	var signed lsp2.GatewayMetadata
	var key ed25519.PublicKey
	verr := errors.New("no JWKS key verifies signed_metadata")
	for _, k := range jwks.Keys {
		x, err := lsp2.UnB64(k.X)
		if k.Kty != "OKP" || k.Crv != "Ed25519" || err != nil || len(x) != ed25519.PublicKeySize || (kid != "" && lsp2.Thumbprint(x) != kid) {
			continue
		}
		if signed, verr = lsp2.VerifyMetadata(x, meta.SignedMetadata, iss, tenant, time.Now()); verr == nil {
			key = x
			break
		}
	}
	if key == nil {
		return nil, fmt.Errorf("signed gateway metadata: %w", verr)
	}
	if signed.SaaS.PairingURI != "" {
		linkURL = signed.SaaS.PairingURI
	}
	if linkURL != "" {
		u, err := url.Parse(linkURL)
		if err != nil || (u.Scheme != "https" && !(u.Scheme == "http" && lsp2.IsLoopbackHost(u.Hostname()))) {
			return nil, errors.New("pairing page must be https")
		}
	}
	return &PairRequest{hc: hc, meta: signed, gwKey: key, remoteURL: remote, linkURL: linkURL, Consent: Consent{
		SaaS: *signed.SaaS, Gateway: iss, Tenant: tenant, Audience: signed.Audience, RequestedModels: append([]string{}, signed.RequestedModels...),
		RequestedScopes: append([]string{}, signed.RequestedScopes...), GatewayKeyID: lsp2.Thumbprint(key), Remote: remote, Verified: true}}, nil
}

func jwsKid(jws string) string {
	h, _, _ := strings.Cut(jws, ".")
	b, err := lsp2.UnB64(h)
	if err != nil {
		return ""
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	k, _ := m["kid"].(string)
	return k
}

// Deny records the user's refusal; nothing is sent to the gateway.
func (r *PairRequest) Deny() error {
	r.decided = true
	return &ConsentError{Reason: "the user declined pairing with " + r.Consent.SaaS.Name}
}

// Approve must only be called after the user explicitly approved Consent. It
// generates the device key and requests a user code (RFC 8628).
func (r *PairRequest) Approve(ctx context.Context, deviceName string) (*Pairing, error) {
	if r.decided {
		return nil, errors.New("pairing request already decided")
	}
	r.decided = true
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	var auth struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
		ExpiresIn  int    `json:"expires_in"`
		Interval   int    `json:"interval"`
	}
	st, err := postForm(ctx, r.hc, r.meta.DeviceAuthorizationEndpoint, map[string]string{"tenant": r.Consent.Tenant, "public_key": lsp2.B64(pub), "device_name": deviceName}, &auth)
	if err != nil || st != 200 || auth.DeviceCode == "" {
		return nil, fmt.Errorf("device authorization failed: %v (HTTP %d)", err, st)
	}
	if auth.Interval < 1 {
		auth.Interval = 5
	}
	link := r.linkURL
	if link != "" {
		sep := "?"
		if strings.Contains(link, "?") {
			sep = "&"
		}
		// jkt lets the SaaS pin this device key (and through it the E2EE key).
		link += sep + "user_code=" + url.QueryEscape(auth.UserCode) + "&jkt=" + url.QueryEscape(lsp2.Thumbprint(pub))
	}
	return &Pairing{UserCode: auth.UserCode, LinkURL: link, JKT: lsp2.Thumbprint(pub), Consent: r.Consent, ExpiresAt: time.Now().Add(time.Duration(auth.ExpiresIn) * time.Second),
		req: r, key: key, code: auth.DeviceCode, interval: time.Duration(auth.Interval) * time.Second}, nil
}

// StartPairing is Prepare + Approve without a consent screen. Use it only for
// automation where the operator already consented (tests, the mock, scripted
// CLI runs); interactive apps MUST show Consent first.
func StartPairing(ctx context.Context, hc *http.Client, remoteURL, deviceName string) (*Pairing, error) {
	r, err := Prepare(ctx, hc, remoteURL)
	if err != nil {
		return nil, err
	}
	return r.Approve(ctx, deviceName)
}

// Pairing is an in-progress RFC 8628 device authorization.
type Pairing struct {
	UserCode  string
	LinkURL   string
	JKT       string // RFC 7638 thumbprint of the new device key
	Consent   Consent
	ExpiresAt time.Time
	req       *PairRequest
	key       ed25519.PrivateKey
	code      string
	interval  time.Duration
}

// Wait polls the token endpoint until the user approves, denies or the code expires.
func (p *Pairing) Wait(ctx context.Context) (Identity, error) {
	r := p.req
	for time.Now().Before(p.ExpiresAt) {
		select {
		case <-ctx.Done():
			return Identity{}, ctx.Err()
		case <-time.After(p.interval):
		}
		var tok map[string]any
		proof := lsp2.SignProof(p.key, "", lsp2.ProofToken, p.code, time.Now())
		st, err := postForm(ctx, r.hc, r.meta.TokenEndpoint, map[string]string{"grant_type": "urn:ietf:params:oauth:grant-type:device_code", "device_code": p.code, "proof": proof}, &tok)
		if err != nil {
			continue
		}
		if st == 200 {
			gk, _ := tok["gateway_key"].(map[string]any)
			x, _ := gk["x"].(string)
			if x != lsp2.B64(r.gwKey) {
				return Identity{}, errors.New("gateway token key differs from the key that signed its metadata")
			}
			at, _ := tok["access_token"].(string)
			id := Identity{RemoteURL: r.remoteURL, GatewayURL: r.Consent.Gateway, Tenant: r.Consent.Tenant, DeviceID: fmt.Sprint(tok["device_id"]), PrivateKey: lsp2.B64(p.key), GatewayKey: x,
				Issuer: r.Consent.Gateway, Audience: r.Consent.Audience, SaaSName: r.Consent.SaaS.Name}
			if _, err := verifyAccessToken(id, r.gwKey, p.key.Public().(ed25519.PublicKey), at); err != nil {
				return Identity{}, err
			}
			xpriv, _ := lsp2.NewX25519()
			id.E2EEKey = lsp2.B64(xpriv)
			if !id.Valid() {
				return Identity{}, errors.New("invalid token response")
			}
			return id, nil
		}
		switch tok["error"] {
		case "authorization_pending":
		case "slow_down":
			p.interval += 5 * time.Second
		case "access_denied":
			return Identity{}, &ConsentError{Reason: "pairing was denied on " + r.Consent.SaaS.Name}
		case "expired_token":
			return Identity{}, errors.New("pairing code expired")
		default:
			return Identity{}, fmt.Errorf("pairing failed: %v", tok["error"])
		}
	}
	return Identity{}, errors.New("pairing code expired")
}

// ErrGatewayIdentity means a token's iss/aud/tenant/subject/key binding does
// not match the configured gateway; the client stops instead of retrying.
var ErrGatewayIdentity = errors.New("gateway identity mismatch")

// verifyAccessToken checks an access token against the pinned gateway key and
// the identity's issuer, audience, tenant, device id and device key.
func verifyAccessToken(id Identity, gw, devPub ed25519.PublicKey, tok string) (lsp2.Claims, error) {
	c, err := lsp2.VerifyJWT(gw, tok, lsp2.TypAccessToken, id.Audience, time.Now())
	if err != nil {
		return c, fmt.Errorf("%w: %v", ErrGatewayIdentity, err)
	}
	switch {
	case c.Iss != id.Issuer:
		return c, fmt.Errorf("%w: token iss %q, expected %q", ErrGatewayIdentity, c.Iss, id.Issuer)
	case c.Tenant != id.Tenant:
		return c, fmt.Errorf("%w: token tenant %q, expected %q", ErrGatewayIdentity, c.Tenant, id.Tenant)
	case c.Sub != id.DeviceID:
		return c, fmt.Errorf("%w: token subject is not this device", ErrGatewayIdentity)
	case c.Cnf["jkt"] != lsp2.Thumbprint(devPub):
		return c, fmt.Errorf("%w: token not bound to this device key", ErrGatewayIdentity)
	}
	return c, nil
}

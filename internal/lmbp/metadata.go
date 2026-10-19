package lmbp

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Gateway identity binding and pairing consent (docs/lmbp-spec.md §18–§19).

const (
	// MetadataPath is the gateway metadata document (RFC 8615 well-known URI,
	// modelled on RFC 8414 authorization server metadata).
	MetadataPath = "/.well-known/lmbp"
	// DiscoveryPath is the SaaS discovery document (§6.1).
	DiscoveryPath = "/.well-known/avora"
	// TypMetadata is the JWS typ of signed_metadata.
	TypMetadata = "lmbp-metadata+jwt"
	// ErrConsentDenied: the local user declined (or never confirmed) pairing
	// with this SaaS. Emitted by device-local APIs and, if the SaaS tries to
	// approve a request the device withdrew, by the gateway.
	ErrConsentDenied = "CONSENT_DENIED"
	// PairLinkScheme is the custom URI scheme of the desktop pairing deep link.
	PairLinkScheme = "lmbp"
	// MetadataMaxAge bounds signed_metadata validity (exp - iat).
	MetadataMaxAge = 24 * time.Hour
)

// Scopes a SaaS may request at pairing (shown on the consent screen).
const (
	ScopeChat   = "lmbp.chat"
	ScopeTools  = "lmbp.tools"
	ScopeVision = "lmbp.vision"
	ScopeAudio  = "lmbp.audio"
)

var Scopes = []string{ScopeChat, ScopeTools, ScopeVision, ScopeAudio}

// TenantIDPattern restricts tenant identifiers (they appear in audiences and URLs).
var TenantIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// SaaSInfo is the display identity of the SaaS (tenant) shown at pairing.
type SaaSInfo struct {
	Name        string `json:"name"`
	LogoURI     string `json:"logo_uri,omitempty"`
	HomepageURI string `json:"homepage_uri,omitempty"`
	PolicyURI   string `json:"policy_uri,omitempty"` // privacy policy
	TosURI      string `json:"tos_uri,omitempty"`
	PairingURI  string `json:"pairing_uri,omitempty"` // page where the signed-in user confirms the user_code
}

// GatewayMetadata is the body of /.well-known/lmbp. Without
// ?tenant= only the gateway fields are present; with it the tenant block and
// signed_metadata are added. Consumers MUST use the values inside
// signed_metadata (RFC 8414 §2.1) when it is present.
type GatewayMetadata struct {
	Issuer                      string    `json:"issuer"`
	JWKSURI                     string    `json:"jwks_uri"`
	DeviceAuthorizationEndpoint string    `json:"device_authorization_endpoint"`
	TokenEndpoint               string    `json:"token_endpoint"`
	ConnectEndpoint             string    `json:"connect_endpoint"`
	ChatCompletionsEndpoint     string    `json:"chat_completions_endpoint,omitempty"`
	ProtocolVersions            []string  `json:"protocol_versions"`
	Profiles                    []string  `json:"profiles"`
	Capabilities                []string  `json:"capabilities"`
	ScopesSupported             []string  `json:"scopes_supported"`
	Tenant                      string    `json:"tenant,omitempty"`
	Audience                    string    `json:"audience,omitempty"`
	SaaS                        *SaaSInfo `json:"saas,omitempty"`
	RequestedModels             []string  `json:"requested_models,omitempty"`
	RequestedScopes             []string  `json:"requested_scopes,omitempty"`
	Iat                         int64     `json:"iat,omitempty"`
	Exp                         int64     `json:"exp,omitempty"`
	SignedMetadata              string    `json:"signed_metadata,omitempty"`
}

// TokenAudience is the access-token aud: the gateway issuer bound to one
// tenant, so a token minted for SaaS A is useless at SaaS B or at another gateway.
func TokenAudience(issuer, tenant string) string { return issuer + "/tenants/" + tenant }

// AssertionAudience is the aud of a device client assertion (RFC 7523 §3: the token endpoint).
func AssertionAudience(issuer string) string { return issuer + "/lmbp/v1/token" }

// Origin normalizes an https origin (scheme://host[:port], lowercase, no
// path/query/userinfo). http is accepted only for loopback hosts (development).
func Origin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("not an origin: %q", raw)
	}
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	switch scheme {
	case "https":
	case "http":
		if !IsLoopbackHost(u.Hostname()) {
			return "", fmt.Errorf("issuer %q must use https", raw)
		}
	default:
		return "", fmt.Errorf("unsupported scheme in %q", raw)
	}
	if (scheme == "https" && u.Port() == "443") || (scheme == "http" && u.Port() == "80") {
		host = strings.ToLower(u.Hostname())
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
	}
	return scheme + "://" + host, nil
}

func IsLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// SignMetadata signs the tenant metadata claims (signed_metadata is omitted from the payload).
func SignMetadata(key ed25519.PrivateKey, kid string, m GatewayMetadata) string {
	m.SignedMetadata = ""
	b, _ := json.Marshal(m)
	return SignJWS(key, TypMetadata, kid, b)
}

// VerifyMetadata checks signed_metadata against a gateway JWKS key and binds
// it to the expected issuer (the gateway origin the device was pointed at)
// and tenant. It returns the signed claims, which supersede the unsigned ones.
func VerifyMetadata(pub ed25519.PublicKey, jws, issuer, tenant string, now time.Time) (GatewayMetadata, error) {
	var m GatewayMetadata
	p, err := VerifyJWS(pub, jws, TypMetadata)
	if err != nil {
		return m, err
	}
	cs := claimsOf(p)
	if err := cs.require(nil, []string{"issuer", "tenant", "audience"}, nil); err != nil {
		return m, err
	}
	if _, _, err := cs.timeRange(); err != nil {
		return m, err
	}
	if json.Unmarshal(p, &m) != nil {
		return m, errors.New("bad metadata payload")
	}
	if !speaks(m.ProtocolVersions) {
		return m, fmt.Errorf("gateway speaks LMBP %v; this device requires %v (§11)", m.ProtocolVersions, ProtocolVersions)
	}
	if m.Issuer != issuer {
		return m, fmt.Errorf("metadata issuer %q does not match gateway %q", m.Issuer, issuer)
	}
	if m.Tenant != tenant || m.Audience != TokenAudience(issuer, tenant) {
		return m, errors.New("metadata tenant/audience mismatch")
	}
	if m.SaaS == nil || strings.TrimSpace(m.SaaS.Name) == "" {
		return m, errors.New("metadata lacks saas.name")
	}
	if m.Exp == 0 || now.Unix() > m.Exp+30 || now.Unix()+30 < m.Iat || time.Duration(m.Exp-m.Iat)*time.Second > MetadataMaxAge {
		return m, errors.New("metadata expired or validity too long")
	}
	for _, u := range []string{m.JWKSURI, m.DeviceAuthorizationEndpoint, m.TokenEndpoint} {
		if !SameOrigin(u, issuer) {
			return m, fmt.Errorf("endpoint %q is not on the issuer origin", u)
		}
	}
	return m, nil
}

// SameOrigin reports whether u is an URL on the given origin.
func SameOrigin(u, origin string) bool {
	p, err := url.Parse(u)
	if err != nil {
		return false
	}
	o, err := Origin(p.Scheme + "://" + p.Host)
	if err != nil {
		// ws/wss endpoints map to http/https
		s := map[string]string{"ws": "http", "wss": "https"}[p.Scheme]
		if s == "" {
			return false
		}
		if o, err = Origin(s + "://" + p.Host); err != nil {
			return false
		}
	}
	return o == origin
}

// PairLink is a parsed lmbp://pair deep link:
//
//	lmbp://pair?v=2&gateway=<https origin>&tenant=<tenant id>[&remote=<SaaS origin>]
//
// The link carries no secret and no display data: everything shown to the
// user comes from the gateway's signed metadata.
type PairLink struct {
	Gateway string // normalized issuer origin
	Tenant  string
	Remote  string // optional SaaS origin (informational / discovery)
}

// PairLinkVersion is the v parameter of lmbp://pair: the protocol major version.
const PairLinkVersion = "2"

func ParsePairLink(raw string) (PairLink, error) {
	var l PairLink
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != PairLinkScheme || u.Host != "pair" || (u.Path != "" && u.Path != "/") {
		return l, errors.New("not an lmbp://pair link")
	}
	q := u.Query()
	for k, v := range q {
		if len(v) != 1 {
			return l, fmt.Errorf("duplicate parameter %s", k)
		}
	}
	if v := q.Get("v"); v != PairLinkVersion {
		return l, fmt.Errorf("unsupported pair link version %q: LMBP 2.0 requires v=%s", v, PairLinkVersion)
	}
	if l.Gateway, err = Origin(q.Get("gateway")); err != nil {
		return l, err
	}
	if l.Tenant = q.Get("tenant"); !TenantIDPattern.MatchString(l.Tenant) {
		return l, errors.New("invalid tenant")
	}
	if r := q.Get("remote"); r != "" {
		if l.Remote, err = Origin(r); err != nil {
			return l, err
		}
	}
	return l, nil
}

// String renders the canonical link.
func (l PairLink) String() string {
	q := url.Values{"v": {PairLinkVersion}, "gateway": {l.Gateway}, "tenant": {l.Tenant}}
	if l.Remote != "" {
		q.Set("remote", l.Remote)
	}
	return PairLinkScheme + "://pair?" + q.Encode()
}

// speaks reports whether a metadata protocol_versions list shares a version
// with this implementation.
func speaks(vs []string) bool {
	for _, v := range vs {
		for _, w := range ProtocolVersions {
			if v == w {
				return true
			}
		}
	}
	return false
}

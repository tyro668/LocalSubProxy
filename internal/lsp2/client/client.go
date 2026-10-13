// Package client is the device side of protocol v2 (lsp2): RFC 8628 pairing
// with a device-held Ed25519 key, the WebSocket session (signed hello,
// mid-connection token refresh, models.update), resume after reconnects,
// credit-window flow control, blob reassembly, end-to-end encrypted tasks and
// verification of gateway-signed task.run frames. Model execution is
// delegated to an Executor so the desktop core can plug in its adapters.
package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"localsubproxy/internal/lsp2"
)

// Executor runs one verified task. The run it receives is fully resolved:
// E2EE requests are decrypted and lsp2-blob: references are replaced by
// data: URIs (images) or base64 (audio). emit streams text and/or tool-call
// deltas (it blocks while the gateway's flow-control window is full); return
// either done or an error body.
type Executor interface {
	Models() []string
	Execute(ctx context.Context, run lsp2.TaskRun, emit func(lsp2.TaskDelta) error) (*lsp2.TaskDone, *lsp2.ErrorBody)
}

// CapabilityReporter is optionally implemented by executors to advertise
// per-model capability flags (tools, vision, audio).
type CapabilityReporter interface {
	ModelCapabilities() map[string][]string
}

// Identity is what a paired device must persist. PrivateKey must be stored
// securely by the caller (the desktop core encrypts it at rest).
type Identity struct {
	RemoteURL  string `json:"remote_url,omitempty"`
	GatewayURL string `json:"gateway_url"`
	Tenant     string `json:"tenant"`
	DeviceID   string `json:"device_id"`
	PrivateKey string `json:"private_key"`        // base64url Ed25519 private key
	GatewayKey string `json:"gateway_key"`        // pinned gateway signing key (base64url)
	E2EEKey    string `json:"e2ee_key,omitempty"` // base64url X25519 private key for end-to-end encryption
}

func (id Identity) keys() (ed25519.PrivateKey, ed25519.PublicKey, error) {
	k, err1 := lsp2.UnB64(id.PrivateKey)
	g, err2 := lsp2.UnB64(id.GatewayKey)
	if err1 != nil || err2 != nil || len(k) != ed25519.PrivateKeySize || len(g) != ed25519.PublicKeySize || id.DeviceID == "" || id.GatewayURL == "" {
		return nil, nil, errors.New("invalid lsp2 identity")
	}
	return k, g, nil
}

// Valid reports whether the identity has usable keys.
func (id Identity) Valid() bool { _, _, err := id.keys(); return err == nil }

// LoadIdentityFile / SaveIdentityFile are plaintext (0600) helpers for the CLI and mock.
func LoadIdentityFile(path string) (Identity, bool, error) {
	var id Identity
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return id, false, nil
	}
	if err != nil {
		return id, false, err
	}
	if err := json.Unmarshal(b, &id); err != nil || !id.Valid() {
		return id, false, errors.New("corrupt identity file")
	}
	return id, true, nil
}

func SaveIdentityFile(path string, id Identity) error {
	b, _ := json.MarshalIndent(id, "", "  ")
	return os.WriteFile(path, b, 0o600)
}

func defaultHTTP() *http.Client {
	return &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// postForm sends an OAuth-style application/x-www-form-urlencoded request.
func postForm(ctx context.Context, hc *http.Client, u string, in map[string]string, out any) (int, error) {
	f := url.Values{}
	for k, v := range in {
		f.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(f.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
}

// Discovery is the public, secret-free document a SaaS serves at /.well-known/lsp-connector.
type Discovery struct {
	GatewayURL string `json:"gateway_url"`
	Tenant     string `json:"tenant"`
	LinkURL    string `json:"link_url"`
	Protocol   string `json:"protocol"`
}

func Discover(ctx context.Context, hc *http.Client, remoteURL string) (Discovery, error) {
	var d Discovery
	if hc == nil {
		hc = defaultHTTP()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(remoteURL, "/")+"/.well-known/lsp-connector", nil)
	if err != nil {
		return d, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return d, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&d) != nil || d.GatewayURL == "" || d.Tenant == "" {
		return d, fmt.Errorf("remote service does not support lsp2 discovery (HTTP %d)", resp.StatusCode)
	}
	return d, nil
}

// Pairing is an in-progress RFC 8628 device authorization.
type Pairing struct {
	UserCode  string
	LinkURL   string
	JKT       string // RFC 7638 thumbprint of the new device key
	ExpiresAt time.Time
	hc        *http.Client
	remoteURL string
	disc      Discovery
	key       ed25519.PrivateKey
	code      string
	interval  time.Duration
}

// StartPairing generates the device key and requests a user code.
func StartPairing(ctx context.Context, hc *http.Client, remoteURL, deviceName string) (*Pairing, error) {
	if hc == nil {
		hc = defaultHTTP()
	}
	disc, err := Discover(ctx, hc, remoteURL)
	if err != nil {
		return nil, err
	}
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
	st, err := postForm(ctx, hc, disc.GatewayURL+"/v2/device/authorize", map[string]string{"tenant": disc.Tenant, "public_key": lsp2.B64(pub), "device_name": deviceName}, &auth)
	if err != nil || st != 200 || auth.DeviceCode == "" {
		return nil, fmt.Errorf("device authorization failed: %v (HTTP %d)", err, st)
	}
	if auth.Interval < 1 {
		auth.Interval = 5
	}
	link := disc.LinkURL
	if link != "" {
		// jkt lets the SaaS pin this device key (and through it the E2EE key).
		link += "?user_code=" + url.QueryEscape(auth.UserCode) + "&jkt=" + url.QueryEscape(lsp2.Thumbprint(pub))
	}
	return &Pairing{UserCode: auth.UserCode, LinkURL: link, JKT: lsp2.Thumbprint(pub), ExpiresAt: time.Now().Add(time.Duration(auth.ExpiresIn) * time.Second), hc: hc, remoteURL: remoteURL, disc: disc, key: key, code: auth.DeviceCode, interval: time.Duration(auth.Interval) * time.Second}, nil
}

// Wait polls the token endpoint until the user approves, denies or the code expires.
func (p *Pairing) Wait(ctx context.Context) (Identity, error) {
	for time.Now().Before(p.ExpiresAt) {
		select {
		case <-ctx.Done():
			return Identity{}, ctx.Err()
		case <-time.After(p.interval):
		}
		var tok map[string]any
		proof := lsp2.SignProof(p.key, "", lsp2.ProofToken, p.code, time.Now())
		st, err := postForm(ctx, p.hc, p.disc.GatewayURL+"/v2/token", map[string]string{"grant_type": "urn:ietf:params:oauth:grant-type:device_code", "device_code": p.code, "proof": proof}, &tok)
		if err != nil {
			continue
		}
		if st == 200 {
			gk, _ := tok["gateway_key"].(map[string]any)
			x, _ := gk["x"].(string)
			xpriv, _ := lsp2.NewX25519()
			id := Identity{RemoteURL: p.remoteURL, GatewayURL: p.disc.GatewayURL, Tenant: p.disc.Tenant, DeviceID: fmt.Sprint(tok["device_id"]), PrivateKey: lsp2.B64(p.key), GatewayKey: x, E2EEKey: lsp2.B64(xpriv)}
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
			return Identity{}, errors.New("pairing was denied on the remote service")
		case "expired_token":
			return Identity{}, errors.New("pairing code expired")
		default:
			return Identity{}, fmt.Errorf("pairing failed: %v", tok["error"])
		}
	}
	return Identity{}, errors.New("pairing code expired")
}

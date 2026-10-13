// Package client is the device side of LMBP (Local Model Bridge Protocol): RFC 8628 pairing
// with a device-held Ed25519 key, the WebSocket session (signed hello,
// mid-connection token refresh, models.update), resume after reconnects,
// credit-window flow control, blob reassembly, end-to-end encrypted tasks and
// verification of gateway-signed task.run frames. Model execution is
// delegated to an Executor so the desktop core can plug in its adapters.
package client

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
)

// Executor runs one verified task. The run it receives is fully resolved:
// E2EE requests are decrypted and lmbp-blob: references are replaced by
// data: URIs (images) or base64 (audio). emit streams text and/or tool-call
// deltas (it blocks while the gateway's flow-control window is full); return
// either done or an error body.
type Executor interface {
	Models() []string
	Execute(ctx context.Context, run lmbp.TaskRun, emit func(lmbp.TaskDelta) error) (*lmbp.TaskDone, *lmbp.ErrorBody)
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
	// Issuer and Audience bind the identity to one gateway origin and one
	// tenant (§18). Tokens whose iss/aud differ are rejected.
	Issuer   string `json:"issuer"`
	Audience string `json:"audience"`
	SaaSName string `json:"saas_name,omitempty"`
}

func (id Identity) keys() (ed25519.PrivateKey, ed25519.PublicKey, error) {
	k, err1 := lmbp.UnB64(id.PrivateKey)
	g, err2 := lmbp.UnB64(id.GatewayKey)
	if err1 != nil || err2 != nil || len(k) != ed25519.PrivateKeySize || len(g) != ed25519.PublicKeySize || id.DeviceID == "" || id.GatewayURL == "" ||
		id.Issuer == "" || id.Audience != lmbp.TokenAudience(id.Issuer, id.Tenant) {
		return nil, nil, errors.New("invalid lmbp identity")
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
		return d, fmt.Errorf("remote service does not support lmbp discovery (HTTP %d)", resp.StatusCode)
	}
	return d, nil
}

// Package client is the device side of protocol v2 (lsp2): RFC 8628 pairing
// with a device-held Ed25519 key, the WebSocket session (signed hello,
// mid-connection token refresh, models.update), and verification of
// gateway-signed task.run frames. Model execution is delegated to an Executor
// so the desktop core can plug in its existing adapters.
package client

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"localsubproxy/internal/lsp2"
)

// Executor runs one verified task. emit streams text; return either done or an error body.
type Executor interface {
	Models() []string
	Execute(ctx context.Context, run lsp2.TaskRun, emit func(text string) error) (*lsp2.TaskDone, *lsp2.ErrorBody)
}

// Identity is what a paired device must persist. PrivateKey must be stored
// securely by the caller (the desktop core encrypts it at rest).
type Identity struct {
	RemoteURL  string `json:"remote_url,omitempty"`
	GatewayURL string `json:"gateway_url"`
	Tenant     string `json:"tenant"`
	DeviceID   string `json:"device_id"`
	PrivateKey string `json:"private_key"` // base64url Ed25519 private key
	GatewayKey string `json:"gateway_key"` // pinned gateway signing key (base64url)
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

func postJSON(ctx context.Context, hc *http.Client, u string, in, out any) (int, error) {
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
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
	st, err := postJSON(ctx, hc, disc.GatewayURL+"/v2/device/authorize", map[string]string{"tenant": disc.Tenant, "public_key": lsp2.B64(pub), "device_name": deviceName}, &auth)
	if err != nil || st != 200 || auth.DeviceCode == "" {
		return nil, fmt.Errorf("device authorization failed: %v (HTTP %d)", err, st)
	}
	if auth.Interval < 1 {
		auth.Interval = 5
	}
	link := disc.LinkURL
	if link != "" {
		link += "?user_code=" + url.QueryEscape(auth.UserCode)
	}
	return &Pairing{UserCode: auth.UserCode, LinkURL: link, ExpiresAt: time.Now().Add(time.Duration(auth.ExpiresIn) * time.Second), hc: hc, remoteURL: remoteURL, disc: disc, key: key, code: auth.DeviceCode, interval: time.Duration(auth.Interval) * time.Second}, nil
}

// Wait polls the token endpoint until the user approves, denies or the code expires.
func (p *Pairing) Wait(ctx context.Context) (Identity, error) {
	proof := lsp2.B64(ed25519.Sign(p.key, []byte("lsp2.token\n"+p.code)))
	for time.Now().Before(p.ExpiresAt) {
		select {
		case <-ctx.Done():
			return Identity{}, ctx.Err()
		case <-time.After(p.interval):
		}
		var tok map[string]any
		st, err := postJSON(ctx, p.hc, p.disc.GatewayURL+"/v2/token", map[string]string{"grant_type": "urn:ietf:params:oauth:grant-type:device_code", "device_code": p.code, "proof": proof}, &tok)
		if err != nil {
			continue
		}
		if st == 200 {
			gk, _ := tok["gateway_key"].(map[string]any)
			x, _ := gk["x"].(string)
			id := Identity{RemoteURL: p.remoteURL, GatewayURL: p.disc.GatewayURL, Tenant: p.disc.Tenant, DeviceID: fmt.Sprint(tok["device_id"]), PrivateKey: lsp2.B64(p.key), GatewayKey: x}
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

type Config struct {
	Executor       Executor
	MaxConcurrency int
	Client         string
	HTTP           *http.Client
	Logger         *log.Logger
	OnState        func(connected bool)
}

type Client struct {
	cfg  Config
	id   Identity
	key  ed25519.PrivateKey
	gw   ed25519.PublicKey
	mu   sync.Mutex
	runs map[string]context.CancelFunc
	seen map[string]time.Time
	wmu  sync.Mutex
	conn *websocket.Conn
}

func New(cfg Config, id Identity) (*Client, error) {
	k, g, err := id.keys()
	if err != nil {
		return nil, err
	}
	if cfg.Executor == nil {
		return nil, errors.New("executor required")
	}
	if cfg.HTTP == nil {
		cfg.HTTP = defaultHTTP()
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.MaxConcurrency < 1 {
		cfg.MaxConcurrency = 1
	}
	if cfg.Client == "" {
		cfg.Client = "localsubproxy/lsp2"
	}
	return &Client{cfg: cfg, id: id, key: k, gw: g, runs: map[string]context.CancelFunc{}, seen: map[string]time.Time{}}, nil
}

func (c *Client) DeviceID() string { return c.id.DeviceID }

// Running returns the number of executing tasks.
func (c *Client) Running() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.runs) }

// accessToken exchanges a device-key-signed assertion for a short-lived token.
func (c *Client) accessToken(ctx context.Context) (string, lsp2.Claims, error) {
	now := time.Now()
	a := lsp2.SignJWT(c.key, "device", lsp2.Claims{Iss: c.id.DeviceID, Sub: c.id.DeviceID, Aud: "lsp2-gateway/token", Iat: now.Unix(), Exp: now.Add(time.Minute).Unix(), Jti: lsp2.RandomID("a_")})
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	st, err := postJSON(ctx, c.cfg.HTTP, c.id.GatewayURL+"/v2/token", map[string]string{"grant_type": "urn:ietf:params:oauth:grant-type:jwt-bearer", "assertion": a}, &tok)
	if err != nil || st != 200 {
		if tok.Error == "invalid_grant" {
			return "", lsp2.Claims{}, ErrRevoked
		}
		return "", lsp2.Claims{}, fmt.Errorf("token refresh failed: %v %s", err, tok.Error)
	}
	cl, err := lsp2.UnverifiedClaims(tok.AccessToken)
	return tok.AccessToken, cl, err
}

// ErrRevoked means the gateway no longer accepts this device; re-pairing is required.
var ErrRevoked = errors.New("device credential rejected (revoked or unknown); pair again")

// Run keeps one session alive with exponential backoff until ctx ends or the device is revoked.
func (c *Client) Run(ctx context.Context, ready chan<- error) error {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := c.session(ctx, ready)
		c.setState(false)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, ErrRevoked) {
			return err
		}
		c.cfg.Logger.Printf("lsp2: session ended: %v", err)
		if time.Since(start) > 30*time.Second {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
	return ctx.Err()
}

func (c *Client) setState(v bool) {
	if c.cfg.OnState != nil {
		c.cfg.OnState(v)
	}
}

func (c *Client) send(e lsp2.Envelope) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.conn == nil {
		return errors.New("not connected")
	}
	e.V = lsp2.Version
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteJSON(e)
}

// ModelsChanged re-announces the executor's models on the live session.
func (c *Client) ModelsChanged() {
	b, _ := json.Marshal(lsp2.ModelsUpdate{Models: c.cfg.Executor.Models(), MaxConcurrency: c.cfg.MaxConcurrency})
	_ = c.send(lsp2.Envelope{Type: lsp2.TypeModelsUpdate, Data: b})
}

func (c *Client) session(ctx context.Context, ready chan<- error) error {
	tok, claims, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	wsURL := "ws" + strings.TrimPrefix(c.id.GatewayURL, "http") + "/v2/connect"
	d := websocket.Dialer{Subprotocols: []string{lsp2.Subprotocol}, HandshakeTimeout: 10 * time.Second}
	conn, _, err := d.DialContext(ctx, wsURL, http.Header{"Authorization": {"Bearer " + tok}})
	if err != nil {
		return err
	}
	defer conn.Close()
	sctx, scancel := context.WithCancel(ctx)
	defer scancel()
	stop := context.AfterFunc(sctx, func() { conn.Close() })
	defer stop()
	ts := time.Now().Unix()
	hb, _ := json.Marshal(lsp2.Hello{DeviceID: c.id.DeviceID, Client: c.cfg.Client, Models: c.cfg.Executor.Models(), MaxConcurrency: c.cfg.MaxConcurrency,
		TS: ts, Proof: lsp2.B64(ed25519.Sign(c.key, lsp2.HelloSigningInput(c.id.DeviceID, claims.Jti, ts)))})
	c.wmu.Lock()
	c.conn = conn
	c.wmu.Unlock()
	defer func() { c.wmu.Lock(); c.conn = nil; c.wmu.Unlock() }()
	if err := c.send(lsp2.Envelope{Type: lsp2.TypeHello, ID: "h1", Data: hb}); err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	var w lsp2.Envelope
	if err := conn.ReadJSON(&w); err != nil {
		return err
	}
	if w.Type != lsp2.TypeWelcome {
		return fmt.Errorf("handshake rejected: %s", w.Data)
	}
	var welcome lsp2.Welcome
	_ = json.Unmarshal(w.Data, &welcome)
	c.cfg.Logger.Printf("lsp2: connected as %s", c.id.DeviceID)
	c.setState(true)
	if ready != nil {
		select {
		case ready <- nil:
		default:
		}
	}
	go c.refreshLoop(sctx, welcome.TokenExpiresAt)
	conn.SetPingHandler(func(m string) error {
		_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		c.wmu.Lock()
		defer c.wmu.Unlock()
		return conn.WriteControl(websocket.PongMessage, []byte(m), time.Now().Add(5*time.Second))
	})
	for {
		_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		var e lsp2.Envelope
		if err := conn.ReadJSON(&e); err != nil {
			return err
		}
		switch e.Type {
		case lsp2.TypeTaskRun:
			c.onRun(e)
		case lsp2.TypeTaskCancel:
			c.mu.Lock()
			if cancel := c.runs[e.TaskID]; cancel != nil {
				cancel()
			}
			c.mu.Unlock()
		case lsp2.TypePing:
			_ = c.send(lsp2.Envelope{Type: lsp2.TypePong, ID: e.ID})
		case lsp2.TypeSessionToken:
		case lsp2.TypeError:
			c.cfg.Logger.Printf("lsp2: gateway error: %s", e.Data)
		}
	}
}

// refreshLoop swaps in a new access token before the current one expires.
func (c *Client) refreshLoop(ctx context.Context, exp int64) {
	for {
		left := time.Until(time.Unix(exp, 0))
		wait := left - 60*time.Second
		if wait < left/2 {
			wait = left / 2
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		tok, claims, err := c.accessToken(ctx)
		if err != nil {
			c.cfg.Logger.Printf("lsp2: mid-session token refresh failed: %v", err)
			if errors.Is(err, ErrRevoked) {
				return
			}
			exp = time.Now().Add(20 * time.Second).Unix() // retry soon; gateway grants a short grace
			continue
		}
		ts := time.Now().Unix()
		b, _ := json.Marshal(lsp2.SessionToken{AccessToken: tok, TS: ts, Proof: lsp2.B64(ed25519.Sign(c.key, lsp2.HelloSigningInput(c.id.DeviceID, claims.Jti, ts)))})
		if c.send(lsp2.Envelope{Type: lsp2.TypeSessionToken, ID: lsp2.RandomID("t_"), Data: b}) != nil {
			return
		}
		exp = claims.Exp
	}
}

func (c *Client) reject(taskID string, seq int64, code, msg string) {
	b, _ := json.Marshal(lsp2.ErrorBody{Code: code, Message: msg})
	_ = c.send(lsp2.Envelope{Type: lsp2.TypeTaskError, TaskID: taskID, Seq: seq, Data: b})
}

// onRun enforces the local policy before any model call.
func (c *Client) onRun(e lsp2.Envelope) {
	sig, _ := lsp2.UnB64(e.Sig)
	if !ed25519.Verify(c.gw, lsp2.TaskRunSigningInput(e.TaskID, e.Data), sig) {
		c.reject(e.TaskID, 1, lsp2.ErrBadSignature, "task.run signature invalid")
		return
	}
	var run lsp2.TaskRun
	if json.Unmarshal(e.Data, &run) != nil || run.DeviceID != c.id.DeviceID || time.Now().Unix() > run.Exp {
		c.reject(e.TaskID, 1, lsp2.ErrBadRequest, "task not for this device or expired")
		return
	}
	allowed := false
	for _, m := range c.cfg.Executor.Models() {
		allowed = allowed || m == run.Model
	}
	c.mu.Lock()
	now := time.Now()
	for k, v := range c.seen {
		if now.After(v) {
			delete(c.seen, k)
		}
	}
	_, replay := c.seen[run.Nonce]
	busy := len(c.runs) >= c.cfg.MaxConcurrency
	if !allowed || replay || busy {
		c.mu.Unlock()
		code := lsp2.ErrForbidden
		if allowed && !replay {
			code = lsp2.ErrDeviceBusy
		}
		c.reject(e.TaskID, 1, code, "rejected by local policy")
		return
	}
	c.seen[run.Nonce] = time.Unix(run.Exp+60, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	c.runs[e.TaskID] = cancel
	c.mu.Unlock()
	go func() {
		defer func() { c.mu.Lock(); delete(c.runs, e.TaskID); c.mu.Unlock(); cancel() }()
		var seq int64
		var mu sync.Mutex
		emit := func(text string) error {
			mu.Lock()
			defer mu.Unlock()
			seq++
			b, _ := json.Marshal(lsp2.TaskDelta{Text: text})
			return c.send(lsp2.Envelope{Type: lsp2.TypeTaskDelta, TaskID: e.TaskID, Seq: seq, Data: b})
		}
		done, fail := c.cfg.Executor.Execute(ctx, run, emit)
		mu.Lock()
		seq++
		final := seq
		mu.Unlock()
		if fail == nil && ctx.Err() != nil {
			fail = &lsp2.ErrorBody{Code: lsp2.ErrCancelled, Message: "cancelled"}
		}
		if fail != nil {
			b, _ := json.Marshal(fail)
			_ = c.send(lsp2.Envelope{Type: lsp2.TypeTaskError, TaskID: e.TaskID, Seq: final, Data: b})
			return
		}
		if done == nil {
			done = &lsp2.TaskDone{FinishReason: "stop"}
		}
		b, _ := json.Marshal(done)
		_ = c.send(lsp2.Envelope{Type: lsp2.TypeTaskDone, TaskID: e.TaskID, Seq: final, Data: b})
	}()
}

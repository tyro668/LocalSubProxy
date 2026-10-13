// Package connector is the reference protocol-v2 device client: it pairs via
// RFC 8628 with a device-held Ed25519 key, connects over the lsp2 WebSocket,
// verifies gateway-signed task.run frames and executes them against a local
// OpenAI-compatible endpoint (LM Studio, Ollama /v1, llama.cpp server, ...).
package connector

import (
	"bufio"
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
	"localsubproxy/mock/v2proto"
)

// Identity is persisted (0600) after pairing. The private key never leaves the device.
type Identity struct {
	GatewayURL string `json:"gateway_url"`
	Tenant     string `json:"tenant"`
	DeviceID   string `json:"device_id"`
	PrivateKey string `json:"private_key"` // base64url Ed25519 seed+pub
	GatewayKey string `json:"gateway_key"` // pinned gateway signing key
}

type Config struct {
	SaaSURL        string
	IdentityFile   string
	DeviceName     string
	ModelURL       string // local OpenAI-compatible base, e.g. http://127.0.0.1:1234
	Models         []string
	MaxConcurrency int
	OnUserCode     func(userCode, linkURL string)
	Logger         *log.Logger
	HTTP           *http.Client
}

type Connector struct {
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

func New(cfg Config) *Connector {
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.MaxConcurrency == 0 {
		cfg.MaxConcurrency = 2
	}
	return &Connector{cfg: cfg, runs: map[string]context.CancelFunc{}, seen: map[string]time.Time{}}
}

func (c *Connector) DeviceID() string { return c.id.DeviceID }

func (c *Connector) postJSON(ctx context.Context, u string, in, out any) (int, error) {
	b, _ := json.Marshal(in)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
}

// Load reads a saved identity; returns false if the device is not paired yet.
func (c *Connector) Load() (bool, error) {
	b, err := os.ReadFile(c.cfg.IdentityFile)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, &c.id); err != nil {
		return false, err
	}
	k, err1 := v2proto.UnB64(c.id.PrivateKey)
	g, err2 := v2proto.UnB64(c.id.GatewayKey)
	if err1 != nil || err2 != nil || len(k) != ed25519.PrivateKeySize || len(g) != ed25519.PublicKeySize {
		return false, errors.New("corrupt identity file")
	}
	c.key, c.gw = k, g
	return true, nil
}

// Pair runs discovery + RFC 8628 device authorization and saves the identity.
func (c *Connector) Pair(ctx context.Context) error {
	var disc struct {
		GatewayURL string `json:"gateway_url"`
		Tenant     string `json:"tenant"`
		LinkURL    string `json:"link_url"`
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.cfg.SaaSURL, "/")+"/.well-known/lsp-connector", nil)
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return err
	}
	err = json.NewDecoder(resp.Body).Decode(&disc)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || disc.GatewayURL == "" || disc.Tenant == "" {
		return fmt.Errorf("discovery failed (status %d)", resp.StatusCode)
	}
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	var auth struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
		ExpiresIn  int    `json:"expires_in"`
		Interval   int    `json:"interval"`
	}
	if st, err := c.postJSON(ctx, disc.GatewayURL+"/v2/device/authorize", map[string]string{"tenant": disc.Tenant, "public_key": v2proto.B64(pub), "device_name": c.cfg.DeviceName}, &auth); err != nil || st != 200 {
		return fmt.Errorf("device authorization failed: %v (status %d)", err, st)
	}
	if c.cfg.OnUserCode != nil {
		c.cfg.OnUserCode(auth.UserCode, disc.LinkURL+"?user_code="+url.QueryEscape(auth.UserCode))
	}
	proof := v2proto.B64(ed25519.Sign(key, []byte("lsp2.token\n"+auth.DeviceCode)))
	interval := time.Duration(auth.Interval) * time.Second
	deadline := time.Now().Add(time.Duration(auth.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
		var tok map[string]any
		st, err := c.postJSON(ctx, disc.GatewayURL+"/v2/token", map[string]string{"grant_type": "urn:ietf:params:oauth:grant-type:device_code", "device_code": auth.DeviceCode, "proof": proof}, &tok)
		if err != nil {
			continue
		}
		if st == 200 {
			gk, _ := tok["gateway_key"].(map[string]any)
			x, _ := gk["x"].(string)
			c.id = Identity{GatewayURL: disc.GatewayURL, Tenant: disc.Tenant, DeviceID: fmt.Sprint(tok["device_id"]), PrivateKey: v2proto.B64(key), GatewayKey: x}
			c.key = key
			c.gw, _ = v2proto.UnB64(x)
			b, _ := json.MarshalIndent(c.id, "", "  ")
			return os.WriteFile(c.cfg.IdentityFile, b, 0o600)
		}
		switch tok["error"] {
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		default:
			return fmt.Errorf("pairing failed: %v", tok["error"])
		}
	}
	return errors.New("pairing code expired")
}

// accessToken exchanges a device-key-signed assertion for a short-lived token.
func (c *Connector) accessToken(ctx context.Context) (string, string, error) {
	now := time.Now()
	a := v2proto.SignJWT(c.key, "device", v2proto.Claims{Iss: c.id.DeviceID, Sub: c.id.DeviceID, Aud: "lsp2-gateway/token", Iat: now.Unix(), Exp: now.Add(time.Minute).Unix(), Jti: v2proto.RandomID("a_")})
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	st, err := c.postJSON(ctx, c.id.GatewayURL+"/v2/token", map[string]string{"grant_type": "urn:ietf:params:oauth:grant-type:jwt-bearer", "assertion": a}, &tok)
	if err != nil || st != 200 {
		return "", "", fmt.Errorf("token refresh failed: %v %s", err, tok.Error)
	}
	cl, err := v2proto.UnverifiedClaims(tok.AccessToken)
	return tok.AccessToken, cl.Jti, err
}

// Run keeps one session alive with exponential backoff until ctx ends.
// ready (optional) receives nil after each successful handshake.
func (c *Connector) Run(ctx context.Context, ready chan<- error) error {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := c.session(ctx, ready)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.cfg.Logger.Printf("connector: session ended: %v", err)
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

func (c *Connector) send(e v2proto.Envelope) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.conn == nil {
		return errors.New("not connected")
	}
	e.V = v2proto.Version
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteJSON(e)
}

func (c *Connector) session(ctx context.Context, ready chan<- error) error {
	tok, jti, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	wsURL := "ws" + strings.TrimPrefix(c.id.GatewayURL, "http") + "/v2/connect"
	d := websocket.Dialer{Subprotocols: []string{v2proto.Subprotocol}, HandshakeTimeout: 10 * time.Second}
	conn, _, err := d.DialContext(ctx, wsURL, http.Header{"Authorization": {"Bearer " + tok}})
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	ts := time.Now().Unix()
	hb, _ := json.Marshal(v2proto.Hello{DeviceID: c.id.DeviceID, Client: "lspmock-connector/0.1", Models: c.cfg.Models, MaxConcurrency: c.cfg.MaxConcurrency,
		TS: ts, Proof: v2proto.B64(ed25519.Sign(c.key, v2proto.HelloSigningInput(c.id.DeviceID, jti, ts)))})
	c.wmu.Lock()
	c.conn = conn
	c.wmu.Unlock()
	defer func() { c.wmu.Lock(); c.conn = nil; c.wmu.Unlock() }()
	if err := c.send(v2proto.Envelope{Type: v2proto.TypeHello, ID: "h1", Data: hb}); err != nil {
		return err
	}
	var w v2proto.Envelope
	if err := conn.ReadJSON(&w); err != nil {
		return err
	}
	if w.Type != v2proto.TypeWelcome {
		return fmt.Errorf("handshake rejected: %s", w.Data)
	}
	c.cfg.Logger.Printf("connector: connected as %s", c.id.DeviceID)
	if ready != nil {
		select {
		case ready <- nil:
		default:
		}
	}
	for {
		var e v2proto.Envelope
		if err := conn.ReadJSON(&e); err != nil {
			return err
		}
		switch e.Type {
		case v2proto.TypeTaskRun:
			c.onRun(e)
		case v2proto.TypeTaskCancel:
			c.mu.Lock()
			if cancel := c.runs[e.TaskID]; cancel != nil {
				cancel()
			}
			c.mu.Unlock()
		case v2proto.TypePing:
			_ = c.send(v2proto.Envelope{Type: v2proto.TypePong, ID: e.ID})
		case v2proto.TypeError:
			c.cfg.Logger.Printf("connector: gateway error: %s", e.Data)
		}
	}
}

func (c *Connector) reject(taskID string, seq int64, code, msg string) {
	b, _ := json.Marshal(v2proto.ErrorBody{Code: code, Message: msg})
	_ = c.send(v2proto.Envelope{Type: v2proto.TypeTaskError, TaskID: taskID, Seq: seq, Data: b})
}

// onRun enforces the local policy before any model call.
func (c *Connector) onRun(e v2proto.Envelope) {
	sig, _ := v2proto.UnB64(e.Sig)
	if !ed25519.Verify(c.gw, v2proto.TaskRunSigningInput(e.TaskID, e.Data), sig) {
		c.reject(e.TaskID, 1, v2proto.ErrBadSignature, "task.run signature invalid")
		return
	}
	var run v2proto.TaskRun
	if json.Unmarshal(e.Data, &run) != nil || run.DeviceID != c.id.DeviceID || time.Now().Unix() > run.Exp {
		c.reject(e.TaskID, 1, v2proto.ErrBadRequest, "task not for this device or expired")
		return
	}
	allowed := false
	for _, m := range c.cfg.Models {
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
		code := map[bool]string{true: v2proto.ErrDeviceBusy, false: v2proto.ErrForbidden}[busy && allowed && !replay]
		c.reject(e.TaskID, 1, code, "rejected by local policy")
		return
	}
	c.seen[run.Nonce] = time.Unix(run.Exp+60, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	c.runs[e.TaskID] = cancel
	c.mu.Unlock()
	go func() {
		defer func() { c.mu.Lock(); delete(c.runs, e.TaskID); c.mu.Unlock(); cancel() }()
		c.execute(ctx, e.TaskID, run)
	}()
}

func (c *Connector) execute(ctx context.Context, taskID string, run v2proto.TaskRun) {
	var seq int64
	next := func() int64 { seq++; return seq }
	body := map[string]any{"model": run.Model, "messages": run.Messages, "stream": true, "stream_options": map[string]bool{"include_usage": true}}
	if run.MaxTokens > 0 {
		body["max_tokens"] = run.MaxTokens
	}
	if run.Temperature != nil {
		body["temperature"] = *run.Temperature
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.cfg.ModelURL, "/")+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			c.reject(taskID, next(), v2proto.ErrCancelled, "cancelled")
			return
		}
		c.reject(taskID, next(), v2proto.ErrUpstream, "local model unreachable")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		c.reject(taskID, next(), v2proto.ErrUpstream, fmt.Sprintf("local model returned HTTP %d", resp.StatusCode))
		return
	}
	finish := "stop"
	var usage *v2proto.Usage
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		p := strings.TrimPrefix(line, "data: ")
		if p == "[DONE]" {
			break
		}
		var ch struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *v2proto.Usage `json:"usage"`
		}
		if json.Unmarshal([]byte(p), &ch) != nil {
			continue
		}
		if ch.Usage != nil {
			usage = ch.Usage
		}
		for _, x := range ch.Choices {
			if x.Delta.Content != "" {
				db, _ := json.Marshal(v2proto.TaskDelta{Text: x.Delta.Content})
				if c.send(v2proto.Envelope{Type: v2proto.TypeTaskDelta, TaskID: taskID, Seq: next(), Data: db}) != nil {
					return
				}
			}
			if x.FinishReason != nil {
				finish = *x.FinishReason
			}
		}
	}
	if ctx.Err() != nil {
		c.reject(taskID, next(), v2proto.ErrCancelled, "cancelled")
		return
	}
	if sc.Err() != nil {
		c.reject(taskID, next(), v2proto.ErrUpstream, "stream interrupted")
		return
	}
	db, _ := json.Marshal(v2proto.TaskDone{FinishReason: finish, Usage: usage})
	_ = c.send(v2proto.Envelope{Type: v2proto.TypeTaskDone, TaskID: taskID, Seq: next(), Data: db})
}

// Package gateway is the protocol-v2 reference gateway used by the mock stack.
// It fixes the three P0 issues of the v1 reference gateway:
//   - per-device Ed25519 identities, short-lived key-bound JWTs, and task.run
//     frames signed by the gateway's own Ed25519 key (no shared HMAC secret);
//   - multi-tenant device table + RFC 8628 device authorization (the user code
//     is shown on the device and typed into the logged-in SaaS page, so no
//     anonymous endpoint ever reveals a code);
//   - a Store interface (SQLite reference) with retention purge and optional
//     AES-GCM encryption of prompts/outputs at rest.
package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"localsubproxy/internal/lsp2"
	"localsubproxy/internal/lsp2/store"

	"github.com/gorilla/websocket"
)

const (
	Audience       = "lsp2-gateway"
	Issuer         = "lsp2-gateway"
	userCodeChars  = "BCDFGHJKLMNPQRSTVWXZ" // RFC 8628 §6.1 recommendation
	maxFrame       = 256 << 10
	deviceGrant    = "urn:ietf:params:oauth:grant-type:device_code"
	jwtBearerGrant = "urn:ietf:params:oauth:grant-type:jwt-bearer"
)

type Config struct {
	Store        store.Store
	SigningKey   ed25519.PrivateKey // gateway key; generated if nil
	TokenTTL     time.Duration      // default 15m
	CodeTTL      time.Duration      // default 10m
	Retention    time.Duration      // default 7 days; 0 keeps the default, <0 disables purge
	TaskTimeout  time.Duration      // default 10m
	PingInterval time.Duration      // default 20s; also the token-expiry check period
	Logger       *log.Logger
}

type Gateway struct {
	cfg       Config
	kid       string
	pub       ed25519.PublicKey
	mu        sync.Mutex
	sess      map[string]*session // device_id → live session
	jtis      map[string]time.Time
	fail      map[string][]time.Time // tenant → recent failed approvals
	refreshes atomic.Int64
}

// TokenRefreshes counts accepted mid-connection session.token swaps.
func (g *Gateway) TokenRefreshes() int64 { return g.refreshes.Load() }

type session struct {
	deviceID, tenantID, ownerID string
	models                      []string
	maxConc                     int
	conn                        *websocket.Conn
	wmu                         sync.Mutex
	mu                          sync.Mutex
	runs                        map[string]chan lsp2.Envelope
	closed                      chan struct{}
	tokenExp                    int64 // guarded by mu; refreshed via session.token
}

func (s *session) modelList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.models...)
}

func New(cfg Config) (*Gateway, error) {
	if cfg.Store == nil {
		return nil, errors.New("store required")
	}
	if cfg.SigningKey == nil {
		_, k, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		cfg.SigningKey = k
	}
	if cfg.TokenTTL == 0 {
		cfg.TokenTTL = 15 * time.Minute
	}
	if cfg.CodeTTL == 0 {
		cfg.CodeTTL = 10 * time.Minute
	}
	if cfg.Retention == 0 {
		cfg.Retention = 7 * 24 * time.Hour
	}
	if cfg.TaskTimeout == 0 {
		cfg.TaskTimeout = 10 * time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.PingInterval == 0 {
		cfg.PingInterval = 20 * time.Second
	}
	pub := cfg.SigningKey.Public().(ed25519.PublicKey)
	return &Gateway{cfg: cfg, pub: pub, kid: lsp2.Thumbprint(pub), sess: map[string]*session{}, jtis: map[string]time.Time{}, fail: map[string][]time.Time{}}, nil
}

// PublicKey returns the key devices pin to verify task.run frames.
func (g *Gateway) PublicKey() ed25519.PublicKey { return g.pub }

// RunRetention purges finished data older than Retention until ctx ends.
func (g *Gateway) RunRetention(ctx context.Context, every time.Duration) {
	if g.cfg.Retention < 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if n, err := g.cfg.Store.Purge(ctx, time.Now().Add(-g.cfg.Retention)); err != nil {
			g.cfg.Logger.Printf("retention purge: %v", err)
		} else if n > 0 {
			g.cfg.Logger.Printf("retention purge: removed %d tasks", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (g *Gateway) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	m.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/jwk-set+json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []lsp2.JWK{lsp2.PublicJWK(g.pub)}})
	})
	m.HandleFunc("/v2/device/authorize", g.authorize)
	m.HandleFunc("/v2/token", g.token)
	m.HandleFunc("/v2/connect", g.connect)
	m.HandleFunc("/v2/tenant/device-approvals", g.approve)
	m.HandleFunc("/v2/tenant/devices", g.listDevices)
	m.HandleFunc("/v2/tenant/devices/", g.revoke)
	m.HandleFunc("/v1/models", g.models)
	m.HandleFunc("/v1/chat/completions", g.chat)
	return m
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// apiError writes an OpenAI-style error body carrying an lsp2 code.
func apiError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": "lsp2_error", "code": code}})
}

// httpError writes RFC 9457 problem details for gateway endpoints and the
// OpenAI error object for the OpenAI-compatible /v1/* surface.
func httpError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		apiError(w, status, code, msg)
		return
	}
	problem(w, status, code, msg)
}

func problem(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": ProblemBase + strings.ToLower(strings.ReplaceAll(code, "_", "-")),
		"title": code, "status": status, "detail": msg, "code": code})
}

// ProblemBase prefixes RFC 9457 problem type URIs.
const ProblemBase = "https://localsubproxy.dev/problems/"

// params reads an OAuth request either as application/x-www-form-urlencoded
// (RFC 6749 §4.1.3 / RFC 8628 §3.1, normative) or as a JSON object (accepted).
func params(r *http.Request, max int64) (map[string]string, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, max)
	out := map[string]string{}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
		for k, v := range r.PostForm {
			if len(v) != 1 {
				return nil, errors.New("duplicate parameter " + k)
			}
			out[k] = v[0]
		}
		return out, nil
	}
	var m map[string]any
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		return nil, err
	}
	for k, v := range m {
		if sv, ok := v.(string); ok {
			out[k] = sv
		}
	}
	return out, nil
}

func oauthError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, map[string]string{"error": code})
}

func decode(r *http.Request, v any, max int64) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, max)).Decode(v)
}

func sha(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func userCode() string {
	b := make([]byte, 8)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(userCodeChars))))
		b[i] = userCodeChars[n.Int64()]
	}
	return string(b[:4]) + "-" + string(b[4:])
}

func normCode(s string) string {
	s = strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(s), "-", ""), " ", ""))
	if len(s) != 8 {
		return ""
	}
	return s[:4] + "-" + s[4:]
}

// tenantAuth authenticates the SaaS backend (tenant API key) and the end user
// it acts for (X-End-User, asserted by the authenticated tenant backend).
func (g *Gateway) tenantAuth(w http.ResponseWriter, r *http.Request) (store.Tenant, string, bool) {
	a := r.Header.Get("Authorization")
	if !strings.HasPrefix(a, "Bearer ") {
		httpError(w, r, 401, lsp2.ErrUnauthenticated, "tenant API key required")
		return store.Tenant{}, "", false
	}
	t, err := g.cfg.Store.TenantByAPIKeyHash(r.Context(), sha(strings.TrimPrefix(a, "Bearer ")))
	if err != nil {
		httpError(w, r, 401, lsp2.ErrUnauthenticated, "invalid tenant API key")
		return t, "", false
	}
	user := r.Header.Get("X-End-User")
	if user == "" || len(user) > 128 {
		httpError(w, r, 400, lsp2.ErrBadRequest, "X-End-User header required")
		return t, "", false
	}
	return t, user, true
}

// ---------- RFC 8628 device authorization ----------

func (g *Gateway) authorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		oauthError(w, 405, "invalid_request")
		return
	}
	ps, err := params(r, 4096)
	req := struct{ Tenant, PublicKey, DeviceName string }{ps["tenant"], ps["public_key"], ps["device_name"]}
	if err != nil || req.Tenant == "" || len(req.DeviceName) > 100 {
		oauthError(w, 400, "invalid_request")
		return
	}
	pub, err := lsp2.UnB64(req.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		oauthError(w, 400, "invalid_request")
		return
	}
	dc := lsp2.RandomID("dc_")
	d := store.DeviceAuth{DeviceCodeHash: sha(dc), UserCode: userCode(), TenantID: req.Tenant, PublicKey: pub, DeviceName: req.DeviceName,
		Status: "pending", ExpiresAt: time.Now().Add(g.cfg.CodeTTL), Interval: 2}
	if err := g.cfg.Store.CreateDeviceAuth(r.Context(), d); err != nil {
		oauthError(w, 500, "server_error")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"device_code": dc, "user_code": d.UserCode, "expires_in": int(g.cfg.CodeTTL.Seconds()), "interval": d.Interval})
}

func (g *Gateway) approve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, r, 405, lsp2.ErrBadRequest, "POST required")
		return
	}
	t, user, ok := g.tenantAuth(w, r)
	if !ok {
		return
	}
	var req struct {
		UserCode string `json:"user_code"`
		Deny     bool   `json:"deny"`
	}
	if decode(r, &req, 4096) != nil {
		httpError(w, r, 400, lsp2.ErrBadRequest, "invalid JSON")
		return
	}
	if !g.allowAttempt(t.ID) {
		httpError(w, r, 429, lsp2.ErrForbidden, "too many failed codes; wait a minute")
		return
	}
	code := normCode(req.UserCode)
	d, err := g.cfg.Store.DeviceAuthByUserCode(r.Context(), t.ID, code)
	if err != nil || d.Status != "pending" || time.Now().After(d.ExpiresAt) {
		g.recordFailure(t.ID)
		httpError(w, r, 404, lsp2.ErrBadRequest, "code invalid, expired or already used")
		return
	}
	if req.Deny {
		d.Status = "denied"
	} else {
		d.Status, d.OwnerID, d.DeviceID = "approved", user, lsp2.RandomID("dev_")
		if err := g.cfg.Store.CreateDevice(r.Context(), store.Device{ID: d.DeviceID, TenantID: t.ID, OwnerID: user, Name: d.DeviceName, PublicKey: d.PublicKey, Status: "active", CreatedAt: time.Now()}); err != nil {
			httpError(w, r, 500, lsp2.ErrInternal, "storage error")
			return
		}
	}
	if err := g.cfg.Store.UpdateDeviceAuth(r.Context(), d); err != nil {
		httpError(w, r, 500, lsp2.ErrInternal, "storage error")
		return
	}
	writeJSON(w, 200, map[string]string{"status": d.Status, "device_id": d.DeviceID, "device_name": d.DeviceName})
}

func (g *Gateway) allowAttempt(tenant string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	cut := time.Now().Add(-time.Minute)
	keep := g.fail[tenant][:0]
	for _, t := range g.fail[tenant] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	g.fail[tenant] = keep
	return len(keep) < 10
}

func (g *Gateway) recordFailure(tenant string) {
	g.mu.Lock()
	g.fail[tenant] = append(g.fail[tenant], time.Now())
	g.mu.Unlock()
}

func (g *Gateway) issue(d store.Device) map[string]any {
	now := time.Now()
	c := lsp2.Claims{Iss: Issuer, Sub: d.ID, Aud: Audience, Tenant: d.TenantID, Iat: now.Unix(), Exp: now.Add(g.cfg.TokenTTL).Unix(),
		Jti: lsp2.RandomID("jti_"), Cnf: map[string]string{"jkt": lsp2.Thumbprint(d.PublicKey)}}
	return map[string]any{"access_token": lsp2.SignJWT(g.cfg.SigningKey, lsp2.TypAccessToken, g.kid, c), "token_type": "Bearer", "expires_in": int(g.cfg.TokenTTL.Seconds()),
		"device_id": d.ID, "gateway_key": lsp2.PublicJWK(g.pub)}
}

func (g *Gateway) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		oauthError(w, 405, "invalid_request")
		return
	}
	ps, err := params(r, 8192)
	// proof: proof JWT by the device key, aud "lsp2:token", ath = hash(device_code).
	req := struct{ GrantType, DeviceCode, Proof, Assertion string }{ps["grant_type"], ps["device_code"], ps["proof"], ps["assertion"]}
	if err != nil {
		oauthError(w, 400, "invalid_request")
		return
	}
	ctx := r.Context()
	switch req.GrantType {
	case deviceGrant:
		d, err := g.cfg.Store.DeviceAuthByCodeHash(ctx, sha(req.DeviceCode))
		if err != nil {
			oauthError(w, 400, "invalid_grant")
			return
		}
		if pc, err := lsp2.VerifyProof(d.PublicKey, req.Proof, "", lsp2.ProofToken, req.DeviceCode, time.Now()); err != nil || !g.useJTI(pc.Jti, time.Now().Add(3*time.Minute)) {
			oauthError(w, 400, "invalid_grant")
			return
		}
		if time.Now().After(d.ExpiresAt) {
			oauthError(w, 400, "expired_token")
			return
		}
		switch d.Status {
		case "denied":
			oauthError(w, 400, "access_denied")
			return
		case "consumed":
			oauthError(w, 400, "invalid_grant")
			return
		case "pending":
			now := time.Now()
			slow := now.Sub(d.LastPoll) < time.Duration(d.Interval)*time.Second-200*time.Millisecond
			d.LastPoll = now
			if slow {
				d.Interval += 5
			}
			_ = g.cfg.Store.UpdateDeviceAuth(ctx, d)
			if slow {
				oauthError(w, 400, "slow_down")
			} else {
				oauthError(w, 400, "authorization_pending")
			}
			return
		}
		dev, err := g.cfg.Store.GetDevice(ctx, d.DeviceID)
		if err != nil {
			oauthError(w, 500, "server_error")
			return
		}
		d.Status = "consumed" // the device code is single-use
		if err := g.cfg.Store.UpdateDeviceAuth(ctx, d); err != nil {
			oauthError(w, 500, "server_error")
			return
		}
		writeJSON(w, 200, g.issue(dev))
	case jwtBearerGrant:
		// Client assertion signed by the device key (RFC 7523 style). The device
		// private key is the long-lived credential; access tokens stay short.
		uc, err := lsp2.UnverifiedClaims(req.Assertion)
		if err != nil {
			oauthError(w, 400, "invalid_grant")
			return
		}
		dev, err := g.cfg.Store.GetDevice(ctx, uc.Sub)
		if err != nil || dev.Status != "active" {
			oauthError(w, 400, "invalid_grant")
			return
		}
		c, err := lsp2.VerifyJWT(dev.PublicKey, req.Assertion, lsp2.TypAssertion, Audience+"/token", time.Now())
		if err != nil || c.Iss != dev.ID || c.Exp-c.Iat > 300 || c.Jti == "" || !g.useJTI(c.Jti, time.Unix(c.Exp+60, 0)) {
			oauthError(w, 400, "invalid_grant")
			return
		}
		writeJSON(w, 200, g.issue(dev))
	default:
		oauthError(w, 400, "unsupported_grant_type")
	}
}

func (g *Gateway) useJTI(j string, until time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	for k, v := range g.jtis {
		if now.After(v) {
			delete(g.jtis, k)
		}
	}
	if _, seen := g.jtis[j]; seen {
		return false
	}
	g.jtis[j] = until
	return true
}

// ---------- tenant device management ----------

func (g *Gateway) online(id string) *session {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sess[id]
}

func (g *Gateway) listDevices(w http.ResponseWriter, r *http.Request) {
	t, user, ok := g.tenantAuth(w, r)
	if !ok {
		return
	}
	ds, err := g.cfg.Store.ListDevices(r.Context(), t.ID, user)
	if err != nil {
		httpError(w, r, 500, lsp2.ErrInternal, "storage error")
		return
	}
	out := []map[string]any{}
	for _, d := range ds {
		item := map[string]any{"device_id": d.ID, "name": d.Name, "status": d.Status, "created_at": d.CreatedAt.UTC().Format(time.RFC3339), "online": false, "models": []string{}}
		if s := g.online(d.ID); s != nil {
			item["online"], item["models"] = true, s.modelList()
		}
		out = append(out, item)
	}
	writeJSON(w, 200, map[string]any{"devices": out})
}

func (g *Gateway) revoke(w http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/v2/tenant/devices/"), "/revoke")
	if !ok || r.Method != http.MethodPost {
		httpError(w, r, 404, lsp2.ErrBadRequest, "not found")
		return
	}
	t, user, ok := g.tenantAuth(w, r)
	if !ok {
		return
	}
	d, err := g.cfg.Store.GetDevice(r.Context(), id)
	if err != nil || d.TenantID != t.ID || d.OwnerID != user {
		httpError(w, r, 404, lsp2.ErrForbidden, "device not found")
		return
	}
	if err := g.cfg.Store.SetDeviceStatus(r.Context(), id, "revoked"); err != nil {
		httpError(w, r, 500, lsp2.ErrInternal, "storage error")
		return
	}
	if s := g.online(id); s != nil {
		_ = s.conn.Close()
	}
	writeJSON(w, 200, map[string]string{"device_id": id, "status": "revoked"})
}

// ---------- device WebSocket ----------

func (g *Gateway) connect(w http.ResponseWriter, r *http.Request) {
	if !contains(websocket.Subprotocols(r), lsp2.Subprotocol) {
		httpError(w, r, 400, lsp2.ErrVersion, "WebSocket subprotocol lsp2 required")
		return
	}
	if r.Header.Get("Origin") != "" {
		httpError(w, r, 403, lsp2.ErrForbidden, "browser origins are not allowed")
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	claims, err := lsp2.VerifyJWT(g.pub, tok, lsp2.TypAccessToken, Audience, time.Now())
	if err != nil {
		httpError(w, r, 401, lsp2.ErrUnauthenticated, err.Error())
		return
	}
	dev, err := g.cfg.Store.GetDevice(r.Context(), claims.Sub)
	if err != nil || dev.Status != "active" || dev.TenantID != claims.Tenant || claims.Cnf["jkt"] != lsp2.Thumbprint(dev.PublicKey) {
		httpError(w, r, 403, lsp2.ErrForbidden, "device revoked or token not bound to device key")
		return
	}
	up := websocket.Upgrader{Subprotocols: []string{lsp2.Subprotocol}, CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(maxFrame)
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	var env lsp2.Envelope
	var hello lsp2.Hello
	if conn.ReadJSON(&env) != nil || env.Type != lsp2.TypeHello || json.Unmarshal(env.Data, &hello) != nil {
		return
	}
	if env.V != lsp2.Version {
		writeErr(conn, env.ID, lsp2.ErrVersion, "unsupported protocol version")
		return
	}
	pc, perr := lsp2.VerifyProof(dev.PublicKey, hello.Proof, dev.ID, lsp2.ProofHello, tok, time.Now())
	if hello.DeviceID != dev.ID || perr != nil || !g.useJTI(pc.Jti, time.Now().Add(3*time.Minute)) {
		writeErr(conn, env.ID, lsp2.ErrUnauthenticated, "invalid key proof")
		return
	}
	if hello.MaxConcurrency < 1 {
		hello.MaxConcurrency = 1
	}
	if hello.MaxConcurrency > 16 {
		hello.MaxConcurrency = 16
	}
	s := &session{deviceID: dev.ID, tenantID: dev.TenantID, ownerID: dev.OwnerID, models: hello.Models, maxConc: hello.MaxConcurrency, conn: conn, runs: map[string]chan lsp2.Envelope{}, closed: make(chan struct{}), tokenExp: claims.Exp}
	g.mu.Lock()
	old := g.sess[dev.ID]
	g.sess[dev.ID] = s
	g.mu.Unlock()
	if old != nil {
		_ = old.conn.Close()
	}
	defer func() {
		g.mu.Lock()
		if g.sess[dev.ID] == s {
			delete(g.sess, dev.ID)
		}
		g.mu.Unlock()
		close(s.closed)
	}()
	wb, _ := json.Marshal(lsp2.Welcome{SessionID: lsp2.RandomID("sess_"), HeartbeatSeconds: 20, MaxFrameBytes: maxFrame, TokenExpiresAt: claims.Exp, Capabilities: intersect(hello.Capabilities, lsp2.Capabilities)})
	if s.send(lsp2.Envelope{V: lsp2.Version, Type: lsp2.TypeWelcome, ID: env.ID, Data: wb}) != nil {
		return
	}
	g.cfg.Logger.Printf("device %s (tenant %s, owner %s) online, models=%v", dev.ID, dev.TenantID, dev.OwnerID, hello.Models)
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(60 * time.Second)) })
	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	go func() {
		t := time.NewTicker(g.cfg.PingInterval)
		defer t.Stop()
		for {
			select {
			case <-s.closed:
				return
			case <-t.C:
				s.mu.Lock()
				expired := time.Now().Unix() > s.tokenExp+30
				s.mu.Unlock()
				if expired { // token not refreshed in time: drop the session
					_ = conn.Close()
					return
				}
				s.wmu.Lock()
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
				s.wmu.Unlock()
			}
		}
	}()
	for {
		var in lsp2.Envelope
		if err := conn.ReadJSON(&in); err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		switch in.Type {
		case lsp2.TypeTaskDelta, lsp2.TypeTaskDone, lsp2.TypeTaskError:
			s.mu.Lock()
			ch := s.runs[in.TaskID]
			s.mu.Unlock()
			if ch != nil {
				select {
				case ch <- in:
				default: // consumer is far behind; drop the session rather than buffer unboundedly
					return
				}
			}
		case lsp2.TypeSessionToken:
			if !g.refreshSession(context.Background(), s, dev, in) {
				writeErr(conn, in.ID, lsp2.ErrUnauthenticated, "session token rejected")
				return
			}
		case lsp2.TypeModelsUpdate:
			var mu lsp2.ModelsUpdate
			if json.Unmarshal(in.Data, &mu) == nil && len(mu.Models) <= 500 {
				s.mu.Lock()
				s.models = mu.Models
				if mu.MaxConcurrency >= 1 && mu.MaxConcurrency <= 16 {
					s.maxConc = mu.MaxConcurrency
				}
				s.mu.Unlock()
			}
		case lsp2.TypePing:
			_ = s.send(lsp2.Envelope{V: lsp2.Version, Type: lsp2.TypePong, ID: in.ID})
		case lsp2.TypePong:
		default:
			writeErr(conn, in.ID, lsp2.ErrBadRequest, "unknown message type "+in.Type)
		}
	}
}

func writeErr(conn *websocket.Conn, id, code, msg string) {
	b, _ := json.Marshal(lsp2.ErrorBody{Code: code, Message: msg})
	_ = conn.WriteJSON(lsp2.Envelope{V: lsp2.Version, Type: lsp2.TypeError, ID: id, Data: b})
}

func (s *session) send(e lsp2.Envelope) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return s.conn.WriteJSON(e)
}

func absI(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func intersect(a, b []string) []string {
	out := []string{}
	for _, x := range a {
		if contains(b, x) && !contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// RegisterTenant stores a tenant with the SHA-256 of its API key.
func (g *Gateway) RegisterTenant(ctx context.Context, id, name, apiKey string) error {
	if len(apiKey) < 32 {
		return errors.New("tenant API key must be at least 32 characters")
	}
	return g.cfg.Store.PutTenant(ctx, store.Tenant{ID: id, Name: name, APIKeyHash: sha(apiKey)})
}

// refreshSession validates a mid-connection token (same checks as connect plus
// a fresh key proof) and extends the session's token expiry.
func (g *Gateway) refreshSession(ctx context.Context, s *session, dev store.Device, in lsp2.Envelope) bool {
	var st lsp2.SessionToken
	if json.Unmarshal(in.Data, &st) != nil {
		return false
	}
	c, err := lsp2.VerifyJWT(g.pub, st.AccessToken, lsp2.TypAccessToken, Audience, time.Now())
	if err != nil || c.Sub != dev.ID || c.Cnf["jkt"] != lsp2.Thumbprint(dev.PublicKey) {
		return false
	}
	cur, err := g.cfg.Store.GetDevice(ctx, dev.ID)
	if err != nil || cur.Status != "active" {
		return false
	}
	pc, err := lsp2.VerifyProof(dev.PublicKey, st.Proof, dev.ID, lsp2.ProofSessionToken, st.AccessToken, time.Now())
	if err != nil || !g.useJTI(pc.Jti, time.Now().Add(3*time.Minute)) {
		return false
	}
	s.mu.Lock()
	s.tokenExp = c.Exp
	s.mu.Unlock()
	g.refreshes.Add(1)
	b, _ := json.Marshal(lsp2.SessionToken{ExpiresAt: c.Exp})
	return s.send(lsp2.Envelope{V: lsp2.Version, Type: lsp2.TypeSessionToken, ID: in.ID, Data: b}) == nil
}

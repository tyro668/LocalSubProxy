// Package v2proto defines the LocalSubProxy protocol v2 ("lsp2") wire types,
// error codes and the Ed25519 helpers shared by the mock gateway and connector.
// See docs/protocol-v2.md.
package lsp2

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Subprotocol is the WebSocket subprotocol name; Version is the envelope version.
const (
	Subprotocol = "lsp2"
	Version     = 2
)

// Message types. Device→gateway: hello, task.delta, task.done, task.error, pong.
// Gateway→device: welcome, task.run, task.cancel, ping, error.
const (
	TypeHello      = "hello"
	TypeWelcome    = "welcome"
	TypeTaskRun    = "task.run"
	TypeTaskDelta  = "task.delta"
	TypeTaskDone   = "task.done"
	TypeTaskError  = "task.error"
	TypeTaskCancel = "task.cancel"
	TypePing       = "ping"
	TypePong       = "pong"
	TypeError      = "error"
	// TypeSessionToken (device→gateway, answered with the same type) swaps in a
	// fresh access token mid-connection; TypeModelsUpdate re-announces models.
	TypeSessionToken = "session.token"
	TypeModelsUpdate = "models.update"
)

// SessionToken carries a refreshed token plus a key proof over its jti.
type SessionToken struct {
	AccessToken string `json:"access_token,omitempty"`
	TS          int64  `json:"ts,omitempty"`
	Proof       string `json:"proof,omitempty"`
	ExpiresAt   int64  `json:"token_expires_at,omitempty"`
}

type ModelsUpdate struct {
	Models         []string `json:"models"`
	MaxConcurrency int      `json:"max_concurrency,omitempty"`
}

// Envelope is the single frame shape on the wire. Data is kept raw so the
// gateway signature (Sig) covers exactly the bytes that were transmitted.
type Envelope struct {
	V      int             `json:"v"`
	Type   string          `json:"type"`
	ID     string          `json:"id,omitempty"`
	TaskID string          `json:"task_id,omitempty"`
	Seq    int64           `json:"seq,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
	Sig    string          `json:"sig,omitempty"`
}

type Hello struct {
	DeviceID       string   `json:"device_id"`
	Client         string   `json:"client"`
	Models         []string `json:"models"`
	MaxConcurrency int      `json:"max_concurrency"`
	// Proof binds the bearer token to the device key: Ed25519 over
	// "lsp2.hello\n<device_id>\n<token jti>\n<ts>".
	TS    int64  `json:"ts"`
	Proof string `json:"proof"`
}

type Welcome struct {
	SessionID        string `json:"session_id"`
	HeartbeatSeconds int    `json:"heartbeat_seconds"`
	MaxFrameBytes    int    `json:"max_frame_bytes"`
	TokenExpiresAt   int64  `json:"token_expires_at"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type TaskRun struct {
	DeviceID    string    `json:"device_id"`
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
	Exp         int64     `json:"exp"`
	Nonce       string    `json:"nonce"`
}

type TaskDelta struct {
	Text string `json:"text"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type TaskDone struct {
	FinishReason string `json:"finish_reason"`
	Usage        *Usage `json:"usage,omitempty"`
}

type ErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// Error codes (string codes, stable across transports).
const (
	ErrUnauthenticated = "UNAUTHENTICATED"
	ErrForbidden       = "FORBIDDEN"
	ErrDeviceOffline   = "DEVICE_OFFLINE"
	ErrDeviceBusy      = "DEVICE_BUSY"
	ErrModelNotFound   = "MODEL_NOT_FOUND"
	ErrBadRequest      = "BAD_REQUEST"
	ErrUpstream        = "UPSTREAM_ERROR"
	ErrTimeout         = "TIMEOUT"
	ErrCancelled       = "CANCELLED"
	ErrBadSignature    = "BAD_SIGNATURE"
	ErrVersion         = "VERSION_UNSUPPORTED"
	ErrInternal        = "INTERNAL"
)

func B64(b []byte) string            { return base64.RawURLEncoding.EncodeToString(b) }
func UnB64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

func RandomID(prefix string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// Thumbprint is the base64url SHA-256 of the raw Ed25519 public key (used as cnf.jkt).
func Thumbprint(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return B64(h[:])
}

func HelloSigningInput(deviceID, jti string, ts int64) []byte {
	return []byte("lsp2.hello\n" + deviceID + "\n" + jti + "\n" + itoa(ts))
}

func TaskRunSigningInput(taskID string, data []byte) []byte {
	return append([]byte("lsp2.task.run\n"+taskID+"\n"), data...)
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// ---- Minimal EdDSA JWT (RFC 7519 / RFC 8037) ----

type Claims struct {
	Iss    string            `json:"iss"`
	Sub    string            `json:"sub"`
	Aud    string            `json:"aud"`
	Tenant string            `json:"ten,omitempty"`
	Iat    int64             `json:"iat"`
	Exp    int64             `json:"exp"`
	Jti    string            `json:"jti"`
	Cnf    map[string]string `json:"cnf,omitempty"`
}

func SignJWT(key ed25519.PrivateKey, kid string, c Claims) string {
	h, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": kid})
	p, _ := json.Marshal(c)
	in := B64(h) + "." + B64(p)
	return in + "." + B64(ed25519.Sign(key, []byte(in)))
}

// VerifyJWT checks signature, aud and time; leeway 30s.
func VerifyJWT(pub ed25519.PublicKey, tok, aud string, now time.Time) (Claims, error) {
	var c Claims
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return c, errors.New("malformed token")
	}
	var h map[string]string
	hb, err := UnB64(parts[0])
	if err != nil || json.Unmarshal(hb, &h) != nil || h["alg"] != "EdDSA" {
		return c, errors.New("bad token header")
	}
	sig, err := UnB64(parts[2])
	if err != nil || !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return c, errors.New("bad token signature")
	}
	pb, err := UnB64(parts[1])
	if err != nil || json.Unmarshal(pb, &c) != nil {
		return c, errors.New("bad token payload")
	}
	if c.Aud != aud {
		return c, errors.New("wrong audience")
	}
	if now.Unix() > c.Exp+30 || now.Unix()+30 < c.Iat {
		return c, errors.New("token expired")
	}
	return c, nil
}

// UnverifiedClaims reads claims without checking the signature (used only to
// find which device key should verify a client assertion).
func UnverifiedClaims(tok string) (Claims, error) {
	var c Claims
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return c, errors.New("malformed token")
	}
	pb, err := UnB64(parts[1])
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(pb, &c)
}

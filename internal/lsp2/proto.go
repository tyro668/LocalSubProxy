// Package lsp2 is the Go reference implementation of the LocalSubProxy
// protocol v2 ("lsp2") wire format: envelope and message types, error codes,
// and the JOSE profile (EdDSA JWS/JWT, RFC 7638 thumbprints, key-bound proof
// JWTs). The normative spec is docs/protocol-v2.md; schemas live in spec/.
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
	Proof       string `json:"proof,omitempty"`
	ExpiresAt   int64  `json:"token_expires_at,omitempty"`
}

type ModelsUpdate struct {
	Models         []string `json:"models"`
	MaxConcurrency int      `json:"max_concurrency,omitempty"`
}

// Envelope is the single frame shape on the wire. task.run carries no Data;
// its payload travels inside JWS (compact, EdDSA) so no JSON canonicalization
// is ever needed.
type Envelope struct {
	V      int             `json:"v"`
	Type   string          `json:"type"`
	ID     string          `json:"id,omitempty"`
	TaskID string          `json:"task_id,omitempty"`
	Seq    int64           `json:"seq,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
	JWS    string          `json:"jws,omitempty"`
}

type Hello struct {
	DeviceID       string   `json:"device_id"`
	Client         string   `json:"client"`
	Models         []string `json:"models"`
	MaxConcurrency int      `json:"max_concurrency"`
	Capabilities   []string `json:"capabilities,omitempty"`
	// Proof is a proof JWT (aud "lsp2:hello", ath = hash of the access token).
	Proof string `json:"proof"`
}

type Welcome struct {
	SessionID        string   `json:"session_id"`
	HeartbeatSeconds int      `json:"heartbeat_seconds"`
	MaxFrameBytes    int      `json:"max_frame_bytes"`
	TokenExpiresAt   int64    `json:"token_expires_at"`
	Capabilities     []string `json:"capabilities"`
}

// Capabilities this implementation understands (negotiated in hello/welcome).
var Capabilities = []string{"session.token", "models.update"}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// TaskRun is the JWS payload of task.run.
type TaskRun struct {
	TaskID      string    `json:"task_id"`
	Iat         int64     `json:"iat"`
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
	ErrRateLimited     = "RATE_LIMITED"
)

func B64(b []byte) string            { return base64.RawURLEncoding.EncodeToString(b) }
func UnB64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

func RandomID(prefix string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// Thumbprint is the RFC 7638 JWK thumbprint (SHA-256, base64url) of an
// Ed25519 public key, used as cnf.jkt (RFC 7800 / RFC 9449) and as kid.
func Thumbprint(pub ed25519.PublicKey) string {
	h := sha256.Sum256([]byte(`{"crv":"Ed25519","kty":"OKP","x":"` + B64(pub) + `"}`))
	return B64(h[:])
}

// JWK is the RFC 8037 OKP public key representation.
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Kid string `json:"kid,omitempty"`
	Alg string `json:"alg,omitempty"`
	Use string `json:"use,omitempty"`
}

func PublicJWK(pub ed25519.PublicKey) JWK {
	return JWK{Kty: "OKP", Crv: "Ed25519", X: B64(pub), Kid: Thumbprint(pub), Alg: "EdDSA", Use: "sig"}
}

// JWS typ values (explicit typing, RFC 8725 §3.11).
const (
	TypAccessToken = "at+jwt"         // RFC 9068
	TypAssertion   = "JWT"            // RFC 7523 client assertion
	TypProof       = "lsp2-proof+jwt" // device key proof (DPoP-style)
	TypTask        = "lsp2-task+jws"  // gateway-signed task.run payload
)

// Proof audiences.
const (
	ProofToken        = "lsp2:token"
	ProofHello        = "lsp2:hello"
	ProofSessionToken = "lsp2:session.token"
)

// SignJWS produces a compact JWS (RFC 7515) with alg EdDSA over payload bytes.
func SignJWS(key ed25519.PrivateKey, typ, kid string, payload []byte) string {
	h := map[string]string{"alg": "EdDSA", "typ": typ}
	if kid != "" {
		h["kid"] = kid
	}
	hb, _ := json.Marshal(h)
	in := B64(hb) + "." + B64(payload)
	return in + "." + B64(ed25519.Sign(key, []byte(in)))
}

// VerifyJWS checks a compact EdDSA JWS with the expected typ and returns its payload.
func VerifyJWS(pub ed25519.PublicKey, jws, typ string) ([]byte, error) {
	parts := strings.Split(jws, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed JWS")
	}
	var h map[string]any
	hb, err := UnB64(parts[0])
	if err != nil || json.Unmarshal(hb, &h) != nil || h["alg"] != "EdDSA" || h["typ"] != typ {
		return nil, errors.New("bad JWS header")
	}
	if _, crit := h["crit"]; crit {
		return nil, errors.New("unsupported crit header")
	}
	sig, err := UnB64(parts[2])
	if err != nil || len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return nil, errors.New("bad JWS signature")
	}
	return UnB64(parts[1])
}

// SignTaskRun signs a task.run payload; the envelope carries the result in jws.
func SignTaskRun(key ed25519.PrivateKey, kid string, run TaskRun) string {
	b, _ := json.Marshal(run)
	return SignJWS(key, TypTask, kid, b)
}

// VerifyTaskRun verifies the gateway signature and binds it to the envelope task_id.
func VerifyTaskRun(pub ed25519.PublicKey, envTaskID, jws string) (TaskRun, error) {
	var run TaskRun
	p, err := VerifyJWS(pub, jws, TypTask)
	if err != nil {
		return run, err
	}
	if json.Unmarshal(p, &run) != nil || run.TaskID == "" || run.TaskID != envTaskID {
		return run, errors.New("task.run payload/task_id mismatch")
	}
	return run, nil
}

// ProofClaims are the claims of a device key proof JWT.
type ProofClaims struct {
	Iss string `json:"iss"` // device_id ("" before pairing completes)
	Aud string `json:"aud"`
	Iat int64  `json:"iat"`
	Jti string `json:"jti"`
	Ath string `json:"ath"` // base64url(SHA-256(bound credential)), as in RFC 9449
}

func Ath(credential string) string { h := sha256.Sum256([]byte(credential)); return B64(h[:]) }

func SignProof(key ed25519.PrivateKey, deviceID, aud, credential string, now time.Time) string {
	b, _ := json.Marshal(ProofClaims{Iss: deviceID, Aud: aud, Iat: now.Unix(), Jti: RandomID("p_"), Ath: Ath(credential)})
	return SignJWS(key, TypProof, "", b)
}

// VerifyProof checks a proof JWT: signature by pub, aud, iss, ath and |iat-now| <= 60s.
func VerifyProof(pub ed25519.PublicKey, proof, deviceID, aud, credential string, now time.Time) (ProofClaims, error) {
	var c ProofClaims
	p, err := VerifyJWS(pub, proof, TypProof)
	if err != nil {
		return c, err
	}
	if json.Unmarshal(p, &c) != nil || c.Aud != aud || c.Iss != deviceID || c.Jti == "" || c.Ath != Ath(credential) {
		return c, errors.New("proof claims invalid")
	}
	if d := now.Unix() - c.Iat; d > 60 || d < -60 {
		return c, errors.New("proof iat outside allowed skew")
	}
	return c, nil
}

// ---- Minimal EdDSA JWT (RFC 7519 / RFC 8037) ----

type Claims struct {
	Iss    string            `json:"iss"`
	Sub    string            `json:"sub"`
	Aud    string            `json:"aud"`
	Tenant string            `json:"tenant,omitempty"`
	Iat    int64             `json:"iat"`
	Exp    int64             `json:"exp"`
	Jti    string            `json:"jti"`
	Cnf    map[string]string `json:"cnf,omitempty"`
}

func SignJWT(key ed25519.PrivateKey, typ, kid string, c Claims) string {
	p, _ := json.Marshal(c)
	return SignJWS(key, typ, kid, p)
}

// VerifyJWT checks signature, typ, aud and time; leeway 30s.
func VerifyJWT(pub ed25519.PublicKey, tok, typ, aud string, now time.Time) (Claims, error) {
	var c Claims
	pb, err := VerifyJWS(pub, tok, typ)
	if err != nil {
		return c, err
	}
	if json.Unmarshal(pb, &c) != nil {
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

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

// Message types. Data frames (sequenced, flow-controlled, resumable):
// gateway→device task.run, blob.chunk; device→gateway task.delta, task.done,
// task.error. Everything else is a control frame.
const (
	TypeHello        = "hello"
	TypeWelcome      = "welcome"
	TypeTaskRun      = "task.run"
	TypeBlobChunk    = "blob.chunk"
	TypeTaskDelta    = "task.delta"
	TypeTaskDone     = "task.done"
	TypeTaskError    = "task.error"
	TypeTaskAck      = "task.ack"
	TypeTaskCancel   = "task.cancel"
	TypePing         = "ping"
	TypePong         = "pong"
	TypeError        = "error"
	TypeSessionToken = "session.token"
	TypeModelsUpdate = "models.update"
)

// IsDataFrame reports whether frames of this type carry a per-task seq, count
// against flow-control windows and are retransmitted on resume.
func IsDataFrame(t string) bool {
	switch t {
	case TypeTaskRun, TypeBlobChunk, TypeTaskDelta, TypeTaskDone, TypeTaskError:
		return true
	}
	return false
}

// Capability names negotiated in hello/welcome.
const (
	CapSessionToken = "session.token"
	CapModelsUpdate = "models.update"
	CapResume       = "resume"
	CapFlow         = "flow"
	CapTools        = "tools"
	CapMultimodal   = "multimodal"
	CapE2EE         = "e2ee"
)

// Capabilities this implementation understands.
var Capabilities = []string{CapSessionToken, CapModelsUpdate, CapResume, CapFlow, CapTools, CapMultimodal, CapE2EE}

// Per-model capability flags (hello/models.update model_capabilities).
const (
	ModelCapTools  = "tools"
	ModelCapVision = "vision"
	ModelCapAudio  = "audio"
)

// Protocol defaults (docs/protocol-v2.md §10).
const (
	DefaultMaxFrameBytes = 256 << 10
	DefaultTaskWindow    = 1 << 20
	DefaultConnWindow    = 4 << 20
	DefaultResumeSeconds = 30
	BlobChunkBytes       = 96 << 10 // raw bytes per blob.chunk (base64url ≈128 KiB)
	MaxBlobBytes         = 20 << 20
	MaxTaskBlobBytes     = 32 << 20
	InlineDataURIBytes   = 16 << 10 // data: URIs above this are moved to blobs
	BlobScheme           = "lsp2-blob:"
)

// SessionToken carries a refreshed token plus a key proof over its jti.
type SessionToken struct {
	AccessToken string `json:"access_token,omitempty"`
	Proof       string `json:"proof,omitempty"`
	ExpiresAt   int64  `json:"token_expires_at,omitempty"`
}

type ModelsUpdate struct {
	Models            []string            `json:"models"`
	ModelCapabilities map[string][]string `json:"model_capabilities,omitempty"`
	MaxConcurrency    int                 `json:"max_concurrency,omitempty"`
	E2EEKey           string              `json:"e2ee_key,omitempty"`
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

// Flow advertises the sender's receive windows (bytes of unacknowledged data
// frames the peer may have outstanding).
type Flow struct {
	TaskWindow int `json:"task_window"`
	ConnWindow int `json:"conn_window"`
}

// ResumeTask is one task's position in a resume exchange: RecvSeq is the
// highest contiguous seq received from the peer for that task.
type ResumeTask struct {
	TaskID  string `json:"task_id"`
	RecvSeq int64  `json:"recv_seq"`
}

type HelloResume struct {
	Token string       `json:"token"`
	Tasks []ResumeTask `json:"tasks"`
}

type Hello struct {
	DeviceID          string              `json:"device_id"`
	Client            string              `json:"client"`
	Models            []string            `json:"models"`
	ModelCapabilities map[string][]string `json:"model_capabilities,omitempty"`
	MaxConcurrency    int                 `json:"max_concurrency"`
	Capabilities      []string            `json:"capabilities,omitempty"`
	Flow              *Flow               `json:"flow,omitempty"`
	Resume            *HelloResume        `json:"resume,omitempty"`
	E2EEKey           string              `json:"e2ee_key,omitempty"`
	// Proof is a proof JWT (aud "lsp2:hello", ath = hash of the access token).
	Proof string `json:"proof"`
}

type WelcomeResume struct {
	Resumed bool         `json:"resumed"`
	Tasks   []ResumeTask `json:"tasks"`
}

type Welcome struct {
	SessionID        string         `json:"session_id"`
	HeartbeatSeconds int            `json:"heartbeat_seconds"`
	MaxFrameBytes    int            `json:"max_frame_bytes"`
	TokenExpiresAt   int64          `json:"token_expires_at"`
	Capabilities     []string       `json:"capabilities"`
	MaxConcurrency   int            `json:"max_concurrency,omitempty"`
	Flow             *Flow          `json:"flow,omitempty"`
	ResumeToken      string         `json:"resume_token,omitempty"`
	ResumeSeconds    int            `json:"resume_window_seconds,omitempty"`
	Resume           *WelcomeResume `json:"resume,omitempty"`
}

// Message is an OpenAI Chat Completions message (text, content parts, tool calls).
type Message struct {
	Role       string     `json:"role"`
	Content    Content    `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Content is either a plain string, an array of content parts, or null.
type Content struct {
	Text  string
	Parts []ContentPart
	Null  bool
}

func Text(s string) Content { return Content{Text: s} }

func (c Content) MarshalJSON() ([]byte, error) {
	switch {
	case c.Parts != nil:
		return json.Marshal(c.Parts)
	case c.Null:
		return []byte("null"), nil
	}
	return json.Marshal(c.Text)
}

func (c *Content) UnmarshalJSON(b []byte) error {
	*c = Content{}
	t := strings.TrimSpace(string(b))
	switch {
	case t == "null":
		c.Null = true
		return nil
	case strings.HasPrefix(t, "["):
		c.Parts = []ContentPart{}
		return json.Unmarshal(b, &c.Parts)
	}
	return json.Unmarshal(b, &c.Text)
}

// PlainText concatenates text content (parts of other types are skipped).
func (c Content) PlainText() string {
	if c.Parts == nil {
		return c.Text
	}
	var b strings.Builder
	for _, p := range c.Parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

type ContentPart struct {
	Type       string      `json:"type"` // text | image_url | input_audio
	Text       string      `json:"text,omitempty"`
	ImageURL   *ImageURL   `json:"image_url,omitempty"`
	InputAudio *InputAudio `json:"input_audio,omitempty"`
}

type ImageURL struct {
	URL    string `json:"url"` // https://, data:<mime>;base64,..., or lsp2-blob:<id> on the tunnel
	Detail string `json:"detail,omitempty"`
}

type InputAudio struct {
	Data   string `json:"data"` // base64 (or lsp2-blob:<id> on the tunnel)
	Format string `json:"format"`
}

type Tool struct {
	Type     string      `json:"type"` // function
	Function FunctionDef `json:"function"`
}

type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ToolCall is used both in assistant messages and (with Index) in stream deltas.
type ToolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// BlobRef describes binary data delivered by blob.chunk frames; it is part of
// the signed task.run payload so the device can verify integrity.
type BlobRef struct {
	ID        string `json:"id"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"` // base64url
	MediaType string `json:"media_type"`
}

// E2EERef points at the sealed request (a JWE compact serialization carried
// as blob media type application/jose).
type E2EERef struct {
	Blob string `json:"blob"`
}

// TaskRun is the JWS payload of task.run.
type TaskRun struct {
	TaskID      string          `json:"task_id"`
	Iat         int64           `json:"iat"`
	DeviceID    string          `json:"device_id"`
	Model       string          `json:"model"`
	Messages    []Message       `json:"messages,omitempty"`
	Tools       []Tool          `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	Blobs       []BlobRef       `json:"blobs,omitempty"`
	E2EE        *E2EERef        `json:"e2ee,omitempty"`
	Exp         int64           `json:"exp"`
	Nonce       string          `json:"nonce"`
}

// BlobChunk (gateway→device data frame).
type BlobChunk struct {
	BlobID string `json:"blob_id"`
	Offset int64  `json:"offset"`
	Data   string `json:"data"` // base64url
	Last   bool   `json:"last,omitempty"`
}

// TaskAck acknowledges every data frame of the task with seq <= Seq.
type TaskAck struct {
	Seq int64 `json:"seq"`
}

type TaskCancel struct {
	Reason string `json:"reason,omitempty"`
}

type TaskDelta struct {
	Text      string     `json:"text,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	E2EE      string     `json:"e2ee,omitempty"` // sealed TaskDelta (E2EE mode)
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type TaskDone struct {
	FinishReason string `json:"finish_reason"`
	Usage        *Usage `json:"usage,omitempty"`
	E2EE         string `json:"e2ee,omitempty"` // sealed {"finish_reason":...} (E2EE mode)
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
	// Extensions (resume, flow control, tools/multimodal, E2EE, idempotency).
	ErrFlowControl     = "FLOW_CONTROL"
	ErrUnsupported     = "CAPABILITY_UNSUPPORTED"
	ErrPayloadTooLarge = "PAYLOAD_TOO_LARGE"
	ErrIdempotency     = "IDEMPOTENCY_KEY_REUSED"
	ErrOutcomeUnknown  = "OUTCOME_UNKNOWN"
	ErrDecrypt         = "E2EE_DECRYPT_FAILED"
	ErrSlowConsumer    = "SLOW_CONSUMER"
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

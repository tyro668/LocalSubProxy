package lmbp

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// End-to-end encryption profile (docs/lmbp-spec.md §12):
//   - device publishes an X25519 key in an "e2ee key statement" JWS signed by
//     its Ed25519 identity key (typ lmbp-e2ee-key+jwt);
//   - the tenant back end seals the request as a JWE (RFC 7516) with
//     alg ECDH-ES (direct key agreement, RFC 7518 §4.6) over X25519
//     (RFC 8037) and enc A256GCM;
//   - the request carries a fresh 256-bit response key; every result frame is
//     sealed with AES-256-GCM, nonce = 0^32 || BE64(seq), AAD bound to the
//     request id, frame type and seq.

const (
	TypE2EEKey      = "lmbp-e2ee-key+jwt"
	CtyE2EERequest  = "lmbp-request+json"
	E2EEMediaType   = "application/jose"
	e2eeAADPrefix   = "lmbp-e2ee/v1|"
	E2EEMaxAge      = 300 // seconds a sealed request stays acceptable
	E2EEKeyLifetime = 30 * 24 * 3600
)

// X25519Thumbprint is the RFC 7638 thumbprint of an X25519 OKP JWK.
func X25519Thumbprint(pub []byte) string {
	h := sha256.Sum256([]byte(`{"crv":"X25519","kty":"OKP","x":"` + B64(pub) + `"}`))
	return B64(h[:])
}

// E2EEKeyClaims is the payload of a device e2ee key statement.
type E2EEKeyClaims struct {
	Iss string `json:"iss"` // device_id
	Sub string `json:"sub"` // device_id
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	JWK JWK    `json:"jwk"` // kty OKP, crv X25519, use enc, kid = thumbprint
}

func SignE2EEKey(idKey ed25519.PrivateKey, deviceID string, xpub []byte, now time.Time) string {
	c := E2EEKeyClaims{Iss: deviceID, Sub: deviceID, Iat: now.Unix(), Exp: now.Unix() + E2EEKeyLifetime,
		JWK: JWK{Kty: "OKP", Crv: "X25519", X: B64(xpub), Kid: X25519Thumbprint(xpub), Use: "enc", Alg: "ECDH-ES"}}
	b, _ := json.Marshal(c)
	return SignJWS(idKey, TypE2EEKey, Thumbprint(idKey.Public().(ed25519.PublicKey)), b)
}

// VerifyE2EEKey checks a key statement against the device identity key the
// verifier pinned (by RFC 7638 thumbprint, obtained out of band at pairing).
func VerifyE2EEKey(devicePub ed25519.PublicKey, pinnedJKT, deviceID, stmt string, now time.Time) ([]byte, string, error) {
	if pinnedJKT != "" && Thumbprint(devicePub) != pinnedJKT {
		return nil, "", errors.New("device key does not match pinned thumbprint")
	}
	p, err := VerifyJWS(devicePub, stmt, TypE2EEKey)
	if err != nil {
		return nil, "", err
	}
	cs := claimsOf(p)
	if err := cs.require(nil, []string{"iss", "sub"}, nil); err != nil {
		return nil, "", err
	}
	if _, _, err := cs.timeRange(); err != nil {
		return nil, "", err
	}
	var c E2EEKeyClaims
	if json.Unmarshal(p, &c) != nil || c.Sub != deviceID || c.Iss != deviceID || c.JWK.Kty != "OKP" || c.JWK.Crv != "X25519" {
		return nil, "", errors.New("bad e2ee key statement")
	}
	if now.Unix() > c.Exp || now.Unix()+60 < c.Iat {
		return nil, "", errors.New("e2ee key statement expired")
	}
	x, err := UnB64(c.JWK.X)
	if err != nil || len(x) != 32 || c.JWK.Kid != X25519Thumbprint(x) {
		return nil, "", errors.New("bad e2ee key")
	}
	return x, c.JWK.Kid, nil
}

// E2EERequest is the plaintext of the sealed request.
type E2EERequest struct {
	RID         string          `json:"rid"` // request id chosen by the sender; binds response frames
	Iat         int64           `json:"iat"`
	Model       string          `json:"model"`
	Messages    []Message       `json:"messages"`
	Tools       []Tool          `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Stop        []string        `json:"stop,omitempty"`
	ResponseKey string          `json:"response_key"` // base64url 32 bytes (A256GCM)
}

// JWEHeader is the protected header of a sealed request.
type JWEHeader struct {
	Alg string `json:"alg"`
	Enc string `json:"enc"`
	Epk JWK    `json:"epk"`
	Kid string `json:"kid"`
	Cty string `json:"cty"`
}

// concatKDF implements RFC 7518 §4.6.2 (Concat KDF, SHA-256) for a 256-bit key
// with empty apu/apv.
func concatKDF(z []byte, enc string) []byte {
	var buf []byte
	buf = binary.BigEndian.AppendUint32(buf, 1)
	buf = append(buf, z...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(enc)))
	buf = append(buf, enc...)
	buf = binary.BigEndian.AppendUint32(buf, 0) // PartyUInfo
	buf = binary.BigEndian.AppendUint32(buf, 0) // PartyVInfo
	buf = binary.BigEndian.AppendUint32(buf, 256)
	h := sha256.Sum256(buf)
	return h[:]
}

func gcm(key []byte) cipher.AEAD {
	b, _ := aes.NewCipher(key)
	g, _ := cipher.NewGCM(b)
	return g
}

// SealJWE encrypts plaintext to the recipient X25519 key (fresh ephemeral key, random IV).
func SealJWE(recipient []byte, kid string, plaintext []byte) (string, error) {
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	iv := make([]byte, 12)
	_, _ = rand.Read(iv)
	return SealJWEWith(eph.Bytes(), iv, recipient, kid, plaintext)
}

// SealJWEWith is SealJWE with caller-supplied ephemeral private key and IV (test vectors only).
func SealJWEWith(ephPriv, iv, recipient []byte, kid string, plaintext []byte) (string, error) {
	eph, err := ecdh.X25519().NewPrivateKey(ephPriv)
	if err != nil {
		return "", err
	}
	rpub, err := ecdh.X25519().NewPublicKey(recipient)
	if err != nil {
		return "", err
	}
	z, err := eph.ECDH(rpub)
	if err != nil {
		return "", err
	}
	h := JWEHeader{Alg: "ECDH-ES", Enc: "A256GCM", Epk: JWK{Kty: "OKP", Crv: "X25519", X: B64(eph.PublicKey().Bytes())}, Kid: kid, Cty: CtyE2EERequest}
	hb, _ := json.Marshal(h)
	prot := B64(hb)
	ct := gcm(concatKDF(z, "A256GCM")).Seal(nil, iv, plaintext, []byte(prot))
	n := len(ct) - 16
	return prot + ".." + B64(iv) + "." + B64(ct[:n]) + "." + B64(ct[n:]), nil
}

// OpenJWE decrypts a sealed request with the recipient X25519 private key.
func OpenJWE(priv []byte, jwe string) (JWEHeader, []byte, error) {
	var h JWEHeader
	p := strings.Split(jwe, ".")
	if len(p) != 5 || p[1] != "" {
		return h, nil, errors.New("malformed JWE")
	}
	hb, err := UnB64(p[0])
	if err != nil {
		return h, nil, err
	}
	var raw map[string]any
	if json.Unmarshal(hb, &raw) != nil || json.Unmarshal(hb, &h) != nil {
		return h, nil, errors.New("bad JWE header")
	}
	if _, bad := raw["crit"]; bad || raw["zip"] != nil || h.Alg != "ECDH-ES" || h.Enc != "A256GCM" || h.Epk.Kty != "OKP" || h.Epk.Crv != "X25519" {
		return h, nil, errors.New("unsupported JWE algorithm")
	}
	epk, err1 := UnB64(h.Epk.X)
	iv, err2 := UnB64(p[2])
	ct, err3 := UnB64(p[3])
	tag, err4 := UnB64(p[4])
	if err := errors.Join(err1, err2, err3, err4); err != nil || len(iv) != 12 || len(tag) != 16 {
		return h, nil, errors.New("bad JWE encoding")
	}
	k, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		return h, nil, err
	}
	pub, err := ecdh.X25519().NewPublicKey(epk)
	if err != nil {
		return h, nil, err
	}
	z, err := k.ECDH(pub)
	if err != nil {
		return h, nil, err
	}
	pt, err := gcm(concatKDF(z, "A256GCM")).Open(nil, iv, append(ct, tag...), []byte(p[0]))
	if err != nil {
		return h, nil, errors.New("JWE decryption failed")
	}
	return h, pt, nil
}

// ChunkAAD is the additional authenticated data of a sealed result frame.
func ChunkAAD(rid, frameType string, seq int64) []byte {
	return []byte(e2eeAADPrefix + rid + "|" + frameType + "|" + strconv.FormatInt(seq, 10))
}

func chunkNonce(seq int64) []byte {
	n := make([]byte, 12)
	binary.BigEndian.PutUint64(n[4:], uint64(seq))
	return n
}

// SealChunk seals one result frame body; returns base64url(ciphertext||tag).
func SealChunk(key []byte, rid, frameType string, seq int64, plaintext []byte) string {
	return B64(gcm(key).Seal(nil, chunkNonce(seq), plaintext, ChunkAAD(rid, frameType, seq)))
}

func OpenChunk(key []byte, rid, frameType string, seq int64, sealed string) ([]byte, error) {
	b, err := UnB64(sealed)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("bad response key")
	}
	pt, err := gcm(key).Open(nil, chunkNonce(seq), b, ChunkAAD(rid, frameType, seq))
	if err != nil {
		return nil, errors.New("chunk authentication failed")
	}
	return pt, nil
}

// NewX25519 returns a fresh X25519 key pair (private, public).
func NewX25519() ([]byte, []byte) {
	k, _ := ecdh.X25519().GenerateKey(rand.Reader)
	return k.Bytes(), k.PublicKey().Bytes()
}

// X25519Public derives the public key from a private key.
func X25519Public(priv []byte) ([]byte, error) {
	k, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return k.PublicKey().Bytes(), nil
}

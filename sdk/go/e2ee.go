package lmbpserver

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
)

// End-to-end encryption from business code (spec §17). The gateway relays
// the sealed request and the sealed response frames without being able to
// read them; only the business backend and the user's device can.
type (
	E2EERequest = lmbp.E2EERequest // the plaintext that gets sealed
	TaskDelta   = lmbp.TaskDelta   // an opened task.delta frame
)

// E2EESealed is a sealed request plus what is needed to open its response.
// Keep it (in memory) for the lifetime of the call.
type E2EESealed struct {
	JWE         string // put in ChatRequest.E2EE
	RID         string // request id bound into every response frame
	ResponseKey []byte // 32-byte A256GCM key for the response frames
}

// SealE2EERequest seals r to dev's X25519 key. The key statement is verified
// against dev's identity key, which MUST match pinnedJKT (the thumbprint your
// backend recorded at pairing, e.g. ApproveResult.JKT) so a compromised
// gateway cannot substitute its own key. RID, Iat and ResponseKey are filled
// in when empty.
func SealE2EERequest(dev DeviceInfo, pinnedJKT string, r E2EERequest) (*E2EESealed, error) {
	if pinnedJKT == "" {
		return nil, errors.New("lmbp: pinned device thumbprint required")
	}
	if dev.E2EEKey == "" {
		return nil, errors.New("lmbp: device has no e2ee key")
	}
	xpub, kid, err := lmbp.VerifyE2EEKey(ed25519.PublicKey(dev.PublicKey), pinnedJKT, dev.DeviceID, dev.E2EEKey, time.Now())
	if err != nil {
		return nil, err
	}
	if r.RID == "" {
		r.RID = lmbp.RandomID("rid_")
	}
	if r.Iat == 0 {
		r.Iat = time.Now().Unix()
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if k, err := lmbp.UnB64(r.ResponseKey); err == nil && len(k) == 32 {
		key = k
	}
	r.ResponseKey = lmbp.B64(key)
	pt, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	jwe, err := lmbp.SealJWE(xpub, kid, pt)
	if err != nil {
		return nil, err
	}
	return &E2EESealed{JWE: jwe, RID: r.RID, ResponseKey: key}, nil
}

// Open decrypts one sealed response frame (ChatCompletionChunk.E2EE or an
// element of ChatCompletion.E2EE). It fails if the frame was altered,
// reordered (seq/type are authenticated) or belongs to another request.
func (s *E2EESealed) Open(f E2EEFrame) ([]byte, error) {
	return lmbp.OpenChunk(s.ResponseKey, s.RID, f.Type, f.Seq, f.Data)
}

// DecodeE2EEDelta parses an opened task.delta frame.
func DecodeE2EEDelta(p []byte) (TaskDelta, error) {
	var d TaskDelta
	return d, json.Unmarshal(p, &d)
}

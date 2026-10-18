package lmbpserver

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tyro668/avora/internal/lmbp"
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
// element of ChatCompletion.E2EE). It fails if the frame was altered, its
// seq/type were changed, or it belongs to another request. Open is
// stateless: on its own it does NOT detect frames that were reordered,
// duplicated, dropped or truncated. Read a response with Reader instead.
func (s *E2EESealed) Open(f E2EEFrame) ([]byte, error) {
	return lmbp.OpenChunk(s.ResponseKey, s.RID, f.Type, f.Seq, f.Data)
}

// Reader returns a stateful reader for one response (spec §17.5).
func (s *E2EESealed) Reader() *E2EEReader { return s.ReaderAfter(0) }

// ReaderAfter is Reader for a stream resumed after seq (the last sealed
// frame already read, e.g. from a previous reader's Last).
func (s *E2EESealed) ReaderAfter(seq int64) *E2EEReader {
	return &E2EEReader{s: s, last: seq}
}

// E2EEReader opens the sealed frames of one response in order. Next accepts
// only seq last+1 of type task.delta or task.done and nothing after
// task.done; Finish fails unless the sealed task.done was read. Together
// they detect a gateway that reorders, duplicates, drops or truncates frames.
type E2EEReader struct {
	s    *E2EESealed
	last int64
	done bool
	err  error
}

// Next opens f. After an error the reader stays failed.
func (r *E2EEReader) Next(f E2EEFrame) ([]byte, error) {
	if r.err != nil {
		return nil, r.err
	}
	switch {
	case r.done:
		r.err = fmt.Errorf("lmbp e2ee: frame seq %d after task.done", f.Seq)
	case f.Type != lmbp.TypeTaskDelta && f.Type != lmbp.TypeTaskDone:
		r.err = fmt.Errorf("lmbp e2ee: unexpected frame type %q", f.Type)
	case f.Seq != r.last+1:
		r.err = fmt.Errorf("lmbp e2ee: frame seq %d, want %d (reordered, duplicated or dropped)", f.Seq, r.last+1)
	}
	if r.err != nil {
		return nil, r.err
	}
	p, err := r.s.Open(f)
	if err != nil {
		r.err = fmt.Errorf("lmbp e2ee: frame seq %d: %w", f.Seq, err)
		return nil, r.err
	}
	r.last = f.Seq
	r.done = f.Type == lmbp.TypeTaskDone
	return p, nil
}

// Last is the seq of the last frame read (the resume point for ReaderAfter).
func (r *E2EEReader) Last() int64 { return r.last }

// Done reports whether the sealed task.done was read.
func (r *E2EEReader) Done() bool { return r.done }

// Finish ends the response: an error if a frame failed or the sealed
// task.done never arrived (truncation).
func (r *E2EEReader) Finish() error {
	if r.err != nil {
		return r.err
	}
	if !r.done {
		return fmt.Errorf("lmbp e2ee: response ended after seq %d without the sealed task.done (truncated)", r.last)
	}
	return nil
}

// DecodeE2EEDelta parses an opened task.delta frame.
func DecodeE2EEDelta(p []byte) (TaskDelta, error) {
	var d TaskDelta
	return d, json.Unmarshal(p, &d)
}

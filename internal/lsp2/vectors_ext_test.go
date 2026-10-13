package lsp2

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Vectors for the negotiated extensions: e2ee, flow and resume
// (docs/protocol-v2.md §14–§17).

type vecX25519 struct {
	Private            string `json:"private_b64url"`
	Public             string `json:"public_b64url"`
	JWKThumbprintInput string `json:"jwk_thumbprint_input"`
	Kid                string `json:"kid"`
}

type vecKeyStatement struct {
	Compact         string          `json:"compact"`
	SignedBy        string          `json:"signed_by"`
	Payload         json.RawMessage `json:"payload"`
	DeviceID        string          `json:"device_id"`
	PinnedJKT       string          `json:"pinned_jkt"`
	ExpectPublic    string          `json:"expect_x25519_public_b64url"`
	ExpectKid       string          `json:"expect_kid"`
	SignatureInputs string          `json:"signing_input"`
	Signature       string          `json:"signature_b64url"`
}

type vecJWE struct {
	EphemeralPrivate string          `json:"ephemeral_private_b64url"`
	EphemeralPublic  string          `json:"ephemeral_public_b64url"`
	IV               string          `json:"iv_b64url"`
	RecipientKid     string          `json:"recipient_kid"`
	SharedSecret     string          `json:"shared_secret_b64url"`
	KDFInputHex      string          `json:"concat_kdf_input_hex"`
	CEK              string          `json:"cek_b64url"`
	ProtectedHeader  json.RawMessage `json:"protected_header"`
	ProtectedB64     string          `json:"protected_b64url"`
	Plaintext        string          `json:"plaintext_utf8"`
	Compact          string          `json:"compact"`
}

type vecChunk struct {
	ResponseKey string `json:"response_key_b64url"`
	RID         string `json:"rid"`
	Type        string `json:"type"`
	Seq         int64  `json:"seq"`
	Plaintext   string `json:"plaintext_utf8"`
	NonceHex    string `json:"nonce_hex"`
	AAD         string `json:"aad"`
	Sealed      string `json:"sealed_b64url"`
}

type vecE2EENeg struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"` // jwe | chunk | key_statement
	Compact   string `json:"compact,omitempty"`
	PinnedJKT string `json:"pinned_jkt,omitempty"`
	DeviceID  string `json:"device_id,omitempty"`
	RID       string `json:"rid,omitempty"`
	Type      string `json:"type,omitempty"`
	Seq       int64  `json:"seq,omitempty"`
	Sealed    string `json:"sealed_b64url,omitempty"`
	Why       string `json:"must_reject_because"`
}

type vecE2EE struct {
	DeviceX25519 vecX25519       `json:"device_x25519"`
	KeyStatement vecKeyStatement `json:"key_statement"`
	JWE          vecJWE          `json:"jwe"`
	Chunks       []vecChunk      `json:"chunks"`
	Negative     []vecE2EENeg    `json:"negative"`
}

type vecFlowStep struct {
	Op     string `json:"op"` // send | ack | recv
	Task   string `json:"task"`
	Seq    int64  `json:"seq,omitempty"`
	Size   int    `json:"size,omitempty"`
	Expect string `json:"expect,omitempty"`
}

type vecFlow struct {
	Name       string        `json:"name"`
	Role       string        `json:"role"` // sender | receiver
	TaskWindow int           `json:"task_window"`
	ConnWindow int           `json:"conn_window"`
	Steps      []vecFlowStep `json:"steps"`
}

type vecResume struct {
	Name       string  `json:"name"`
	Sent       []int64 `json:"sent"`
	Acked      int64   `json:"acked"`
	PeerRecv   int64   `json:"peer_recv_seq"`
	Retransmit []int64 `json:"retransmit"`
}

func seedBytes(label string, n int) []byte {
	h := sha256.Sum256([]byte(label))
	return h[:n]
}

func genE2EE(devID string, dk []byte, dkJKT string) (*vecE2EE, error) {
	devKey := edFromSeed(dk)
	xpriv := seedBytes("lsp2 test vector: device x25519 key", 32)
	xp, err := X25519Public(xpriv)
	if err != nil {
		return nil, err
	}
	kid := X25519Thumbprint(xp)
	e := &vecE2EE{DeviceX25519: vecX25519{Private: B64(xpriv), Public: B64(xp), Kid: kid,
		JWKThumbprintInput: `{"crv":"X25519","kty":"OKP","x":"` + B64(xp) + `"}`}}

	claims := E2EEKeyClaims{Iss: devID, Sub: devID, Iat: vecIat, Exp: vecIat + E2EEKeyLifetime,
		JWK: JWK{Kty: "OKP", Crv: "X25519", X: B64(xp), Kid: kid, Use: "enc", Alg: "ECDH-ES"}}
	pb, _ := json.Marshal(claims)
	stmt := SignJWS(devKey, TypE2EEKey, dkJKT, pb)
	parts := strings.Split(stmt, ".")
	e.KeyStatement = vecKeyStatement{Compact: stmt, SignedBy: "device", Payload: pb, DeviceID: devID, PinnedJKT: dkJKT,
		ExpectPublic: B64(xp), ExpectKid: kid, SignatureInputs: parts[0] + "." + parts[1], Signature: parts[2]}

	temp := 0.2
	req := E2EERequest{RID: "rid_0192f5e0-7c1a-7cc0-9b4a-0000000000e1", Iat: vecIat, Model: "lm/qwen3-8b",
		Messages:  []Message{{Role: "user", Content: Text("机密：项目代号青鸾")}},
		MaxTokens: 64, Temperature: &temp,
		ResponseKey: B64(seedBytes("lsp2 test vector: response key", 32))}
	var pt bytes.Buffer
	enc := json.NewEncoder(&pt)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(req)
	plain := bytes.TrimRight(pt.Bytes(), "\n")
	eph := seedBytes("lsp2 test vector: jwe ephemeral key", 32)
	iv := seedBytes("lsp2 test vector: jwe iv", 12)
	compact, err := SealJWEWith(eph, iv, xp, kid, plain)
	if err != nil {
		return nil, err
	}
	ek, _ := ecdh.X25519().NewPrivateKey(eph)
	rk, _ := ecdh.X25519().NewPublicKey(xp)
	z, _ := ek.ECDH(rk)
	kdfIn := kdfInput(z, "A256GCM")
	cek := concatKDF(z, "A256GCM")
	jp := strings.Split(compact, ".")
	hb, _ := UnB64(jp[0])
	e.JWE = vecJWE{EphemeralPrivate: B64(eph), EphemeralPublic: B64(ek.PublicKey().Bytes()), IV: B64(iv), RecipientKid: kid,
		SharedSecret: B64(z), KDFInputHex: hex.EncodeToString(kdfIn), CEK: B64(cek), ProtectedHeader: hb, ProtectedB64: jp[0],
		Plaintext: string(plain), Compact: compact}

	rkey, _ := UnB64(req.ResponseKey)
	for _, c := range []struct {
		typ string
		seq int64
		pt  string
	}{
		{TypeTaskDelta, 1, `{"text":"你好"}`},
		{TypeTaskDelta, 2, `{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"上海\"}"}}]}`},
		{TypeTaskDone, 3, `{"finish_reason":"stop"}`},
	} {
		e.Chunks = append(e.Chunks, vecChunk{ResponseKey: req.ResponseKey, RID: req.RID, Type: c.typ, Seq: c.seq, Plaintext: c.pt,
			NonceHex: hex.EncodeToString(chunkNonce(c.seq)), AAD: string(ChunkAAD(req.RID, c.typ, c.seq)),
			Sealed: SealChunk(rkey, req.RID, c.typ, c.seq, []byte(c.pt))})
	}

	c0 := e.Chunks[0]
	flip := func(s string) string {
		b, _ := UnB64(s)
		b[0] ^= 1
		return B64(b)
	}
	kwHeader := B64([]byte(`{"alg":"ECDH-ES+A256KW","enc":"A256GCM","epk":{"kty":"OKP","crv":"X25519","x":"` + B64(ek.PublicKey().Bytes()) + `"},"kid":"` + kid + `","cty":"lsp2-request+json"}`))
	zipHeader := B64([]byte(`{"alg":"ECDH-ES","enc":"A256GCM","zip":"DEF","epk":{"kty":"OKP","crv":"X25519","x":"` + B64(ek.PublicKey().Bytes()) + `"},"kid":"` + kid + `"}`))
	otherDev := SignE2EEKey(edFromSeed(seedBytes("lsp2 test vector: gateway key", 32)), devID, xp, time.Unix(vecIat, 0))
	expiredClaims := claims
	expiredClaims.Iat, expiredClaims.Exp = vecIat-2*E2EEKeyLifetime, vecIat-E2EEKeyLifetime
	eb, _ := json.Marshal(expiredClaims)
	e.Negative = []vecE2EENeg{
		{Name: "jwe_tampered_ciphertext", Kind: "jwe", Compact: jp[0] + ".." + jp[2] + "." + flip(jp[3]) + "." + jp[4], Why: "GCM tag must not verify"},
		{Name: "jwe_tampered_header", Kind: "jwe", Compact: B64(bytes.Replace(hb, []byte(`"cty":"lsp2-request+json"`), []byte(`"cty":"lsp2-request+json" `), 1)) + ".." + jp[2] + "." + jp[3] + "." + jp[4], Why: "protected header is the GCM AAD"},
		{Name: "jwe_key_wrap_alg", Kind: "jwe", Compact: kwHeader + ".AAAA." + jp[2] + "." + jp[3] + "." + jp[4], Why: "only alg ECDH-ES (direct) is allowed"},
		{Name: "jwe_zip", Kind: "jwe", Compact: zipHeader + ".." + jp[2] + "." + jp[3] + "." + jp[4], Why: "zip is forbidden (compression oracle)"},
		{Name: "chunk_wrong_seq", Kind: "chunk", RID: c0.RID, Type: c0.Type, Seq: 2, Sealed: c0.Sealed, Why: "seq is bound into nonce and AAD (reordering)"},
		{Name: "chunk_wrong_type", Kind: "chunk", RID: c0.RID, Type: TypeTaskDone, Seq: 1, Sealed: c0.Sealed, Why: "frame type is bound into AAD"},
		{Name: "chunk_wrong_rid", Kind: "chunk", RID: "rid_other", Type: c0.Type, Seq: 1, Sealed: c0.Sealed, Why: "request id is bound into AAD (cross-request splicing)"},
		{Name: "chunk_tampered", Kind: "chunk", RID: c0.RID, Type: c0.Type, Seq: 1, Sealed: flip(c0.Sealed), Why: "GCM tag must not verify"},
		{Name: "key_statement_wrong_pin", Kind: "key_statement", Compact: stmt, PinnedJKT: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", DeviceID: devID, Why: "device identity key does not match the thumbprint pinned at pairing"},
		{Name: "key_statement_signed_by_other_key", Kind: "key_statement", Compact: otherDev, PinnedJKT: dkJKT, DeviceID: devID, Why: "signature must verify with the pinned device key"},
		{Name: "key_statement_wrong_device", Kind: "key_statement", Compact: stmt, PinnedJKT: dkJKT, DeviceID: "dev_other", Why: "iss/sub must equal the device_id"},
		{Name: "key_statement_expired", Kind: "key_statement", Compact: SignJWS(devKey, TypE2EEKey, dkJKT, eb), PinnedJKT: dkJKT, DeviceID: devID, Why: "exp < now"},
		{Name: "key_statement_wrong_typ", Kind: "key_statement", Compact: SignJWS(devKey, TypProof, "", pb), PinnedJKT: dkJKT, DeviceID: devID, Why: "typ must be lsp2-e2ee-key+jwt"},
	}
	return e, nil
}

func kdfInput(z []byte, enc string) []byte {
	var buf []byte
	buf = append(buf, 0, 0, 0, 1)
	buf = append(buf, z...)
	buf = append(buf, 0, 0, 0, byte(len(enc)))
	buf = append(buf, enc...)
	buf = append(buf, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0)
	return buf
}

func genFlow() []vecFlow {
	s := func(task string, size int, expect string) vecFlowStep {
		return vecFlowStep{Op: "send", Task: task, Size: size, Expect: expect}
	}
	ack := func(task string, seq int64) vecFlowStep { return vecFlowStep{Op: "ack", Task: task, Seq: seq} }
	r := func(task string, seq int64, size int, expect string) vecFlowStep {
		return vecFlowStep{Op: "recv", Task: task, Seq: seq, Size: size, Expect: expect}
	}
	return []vecFlow{
		{Name: "sender_task_window", Role: "sender", TaskWindow: 1000, ConnWindow: 4000, Steps: []vecFlowStep{
			s("a", 400, "send"), s("a", 400, "send"), s("a", 400, "wait"), ack("a", 1), s("a", 400, "send"), s("a", 300, "wait"), ack("a", 3), s("a", 300, "send")}},
		{Name: "sender_conn_window", Role: "sender", TaskWindow: 1000, ConnWindow: 1500, Steps: []vecFlowStep{
			s("a", 800, "send"), s("b", 600, "send"), s("c", 200, "wait"), ack("a", 1), s("c", 200, "send")}},
		{Name: "sender_oversized_frame_into_empty_window", Role: "sender", TaskWindow: 1000, ConnWindow: 4000, Steps: []vecFlowStep{
			s("a", 1500, "send"), s("a", 10, "wait"), ack("a", 1), s("a", 10, "send")}},
		{Name: "receiver_order_duplicates_gaps", Role: "receiver", TaskWindow: 1000, ConnWindow: 4000, Steps: []vecFlowStep{
			r("a", 1, 100, "accepted"), r("a", 2, 100, "accepted"), r("a", 2, 100, "duplicate"), r("a", 1, 100, "duplicate"), r("a", 4, 100, "gap")}},
		{Name: "receiver_window_overflow", Role: "receiver", TaskWindow: 1000, ConnWindow: 4000, Steps: []vecFlowStep{
			r("a", 1, 600, "accepted"), r("a", 2, 600, "overflow")}},
		{Name: "receiver_ack_frees_window", Role: "receiver", TaskWindow: 1000, ConnWindow: 4000, Steps: []vecFlowStep{
			r("a", 1, 600, "accepted"), ack("a", 1), r("a", 2, 600, "accepted")}},
		{Name: "receiver_conn_overflow", Role: "receiver", TaskWindow: 1000, ConnWindow: 1500, Steps: []vecFlowStep{
			r("a", 1, 900, "accepted"), r("b", 1, 900, "overflow")}},
	}
}

func genResume() []vecResume {
	return []vecResume{
		{Name: "retransmit_after_peer_position", Sent: []int64{1, 2, 3, 4, 5, 6, 7}, Acked: 2, PeerRecv: 5, Retransmit: []int64{6, 7}},
		{Name: "peer_behind_last_ack", Sent: []int64{1, 2, 3, 4}, Acked: 3, PeerRecv: 3, Retransmit: []int64{4}},
		{Name: "nothing_lost", Sent: []int64{1, 2, 3}, Acked: 0, PeerRecv: 3, Retransmit: []int64{}},
	}
}

func checkExt(t *testing.T, v vectors) {
	t.Helper()
	now := time.Unix(v.Now, 0)
	e := v.E2EE
	devPub, _ := UnB64(v.Keys["device"].Public)
	// key statement
	x, kid, err := VerifyE2EEKey(devPub, e.KeyStatement.PinnedJKT, e.KeyStatement.DeviceID, e.KeyStatement.Compact, now)
	if err != nil || B64(x) != e.KeyStatement.ExpectPublic || kid != e.KeyStatement.ExpectKid {
		t.Fatalf("e2ee key statement: %v", err)
	}
	// JWE open with device X25519 key
	xpriv, _ := UnB64(e.DeviceX25519.Private)
	h, pt, err := OpenJWE(xpriv, e.JWE.Compact)
	if err != nil || string(pt) != e.JWE.Plaintext || h.Kid != e.DeviceX25519.Kid || h.Cty != CtyE2EERequest {
		t.Fatalf("e2ee jwe: %v", err)
	}
	for _, c := range e.Chunks {
		k, _ := UnB64(c.ResponseKey)
		got, err := OpenChunk(k, c.RID, c.Type, c.Seq, c.Sealed)
		if err != nil || string(got) != c.Plaintext {
			t.Fatalf("e2ee chunk seq %d: %v", c.Seq, err)
		}
	}
	k0, _ := UnB64(e.Chunks[0].ResponseKey)
	for _, n := range e.Negative {
		var err error
		switch n.Kind {
		case "jwe":
			_, _, err = OpenJWE(xpriv, n.Compact)
		case "chunk":
			_, err = OpenChunk(k0, n.RID, n.Type, n.Seq, n.Sealed)
		case "key_statement":
			_, _, err = VerifyE2EEKey(devPub, n.PinnedJKT, n.DeviceID, n.Compact, now)
		}
		if err == nil {
			t.Fatalf("e2ee negative %s accepted", n.Name)
		}
	}
	// flow
	for _, f := range v.Flow {
		fl := &Flow{TaskWindow: f.TaskWindow, ConnWindow: f.ConnWindow}
		switch f.Role {
		case "sender":
			type fr struct {
				seq  int64
				size int
			}
			sent := map[string][]fr{}
			next := map[string]int64{}
			used := func(task string) (int, int) {
				tu, cu := 0, 0
				for k, l := range sent {
					for _, x := range l {
						cu += x.size
						if k == task {
							tu += x.size
						}
					}
				}
				return tu, cu
			}
			for i, st := range f.Steps {
				switch st.Op {
				case "send":
					tu, cu := used(st.Task)
					ok := CanSend(fl, tu, cu, st.Size)
					if ok != (st.Expect == "send") {
						t.Fatalf("flow %s step %d: CanSend=%v want %s", f.Name, i, ok, st.Expect)
					}
					if ok {
						next[st.Task]++
						sent[st.Task] = append(sent[st.Task], fr{next[st.Task], st.Size})
					}
				case "ack":
					l := sent[st.Task]
					for len(l) > 0 && l[0].seq <= st.Seq {
						l = l[1:]
					}
					sent[st.Task] = l
				}
			}
		case "receiver":
			in := NewInbound(fl)
			names := []string{"accepted", "duplicate", "gap", "overflow"}
			for i, st := range f.Steps {
				switch st.Op {
				case "recv":
					if got := names[in.Accept(st.Task, st.Seq, st.Size)]; got != st.Expect {
						t.Fatalf("flow %s step %d: %s want %s", f.Name, i, got, st.Expect)
					}
				case "ack":
					in.Acked(st.Task, st.Seq)
				}
			}
		}
	}
	// resume (against the real Outbox)
	for _, r := range v.Resume {
		o := NewOutbox()
		for range r.Sent {
			if _, err := o.Send(context.Background(), Envelope{Type: TypeTaskDelta, TaskID: "t"}); err != nil {
				t.Fatal(err)
			}
		}
		o.Ack("t", r.Acked)
		var got []int64
		o.Attach(func(b []byte) error {
			var env Envelope
			_ = json.Unmarshal(b, &env)
			got = append(got, env.Seq)
			return nil
		})
		if err := o.Retransmit("t", r.PeerRecv); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(got) != fmt.Sprint(r.Retransmit) && !(len(got) == 0 && len(r.Retransmit) == 0) {
			t.Fatalf("resume %s: retransmitted %v want %v", r.Name, got, r.Retransmit)
		}
	}
}

func edFromSeed(seed []byte) ed25519.PrivateKey { return ed25519.NewKeyFromSeed(seed) }

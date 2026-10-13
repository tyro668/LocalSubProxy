package lmbp

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"
)

func TestJWT(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	tok := SignJWT(key, TypAccessToken, "k", Claims{Sub: "dev_1", Aud: "a", Iat: now.Unix(), Exp: now.Add(time.Minute).Unix(), Jti: "j"})
	if c, err := VerifyJWT(pub, tok, TypAccessToken, "a", now); err != nil || c.Sub != "dev_1" {
		t.Fatal(err)
	}
	if _, err := VerifyJWT(pub, tok, TypAccessToken, "b", now); err == nil {
		t.Fatal("wrong audience accepted")
	}
	if _, err := VerifyJWT(pub, tok, TypAccessToken, "a", now.Add(5*time.Minute)); err == nil {
		t.Fatal("expired accepted")
	}
	parts := strings.Split(tok, ".")
	forged := SignJWT(key, TypAccessToken, "k", Claims{Sub: "dev_2", Aud: "a", Iat: now.Unix(), Exp: now.Add(time.Minute).Unix()})
	if _, err := VerifyJWT(pub, parts[0]+"."+strings.Split(forged, ".")[1]+"."+parts[2], TypAccessToken, "a", now); err == nil {
		t.Fatal("tampered payload accepted")
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := VerifyJWT(other, tok, TypAccessToken, "a", now); err == nil {
		t.Fatal("wrong key accepted")
	}
}

func TestTypConfusionRejected(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	p := SignProof(key, "dev_1", ProofHello, "tok", now)
	if _, err := VerifyJWT(pub, p, TypAccessToken, ProofHello, now); err == nil {
		t.Fatal("proof accepted as access token")
	}
	if _, err := VerifyProof(pub, p, "dev_1", ProofHello, "tok", now); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyProof(pub, p, "dev_1", ProofHello, "other", now); err == nil {
		t.Fatal("proof bound to another credential accepted")
	}
	if _, err := VerifyProof(pub, p, "dev_1", ProofSessionToken, "tok", now); err == nil {
		t.Fatal("proof replayed for another purpose")
	}
	if _, err := VerifyProof(pub, p, "dev_1", ProofHello, "tok", now.Add(2*time.Minute)); err == nil {
		t.Fatal("stale proof accepted")
	}
	j := SignTaskRun(key, "k", TaskRun{TaskID: "t1", DeviceID: "dev_1"})
	if _, err := VerifyTaskRun(pub, "t2", j); err == nil {
		t.Fatal("task id mismatch accepted")
	}
}

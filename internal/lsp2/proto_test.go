package lsp2

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
	tok := SignJWT(key, "k", Claims{Sub: "dev_1", Aud: "a", Iat: now.Unix(), Exp: now.Add(time.Minute).Unix(), Jti: "j"})
	if c, err := VerifyJWT(pub, tok, "a", now); err != nil || c.Sub != "dev_1" {
		t.Fatal(err)
	}
	if _, err := VerifyJWT(pub, tok, "b", now); err == nil {
		t.Fatal("wrong audience accepted")
	}
	if _, err := VerifyJWT(pub, tok, "a", now.Add(5*time.Minute)); err == nil {
		t.Fatal("expired accepted")
	}
	parts := strings.Split(tok, ".")
	forged := SignJWT(key, "k", Claims{Sub: "dev_2", Aud: "a", Iat: now.Unix(), Exp: now.Add(time.Minute).Unix()})
	if _, err := VerifyJWT(pub, parts[0]+"."+strings.Split(forged, ".")[1]+"."+parts[2], "a", now); err == nil {
		t.Fatal("tampered payload accepted")
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := VerifyJWT(other, tok, "a", now); err == nil {
		t.Fatal("wrong key accepted")
	}
}

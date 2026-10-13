package client

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lsp2"
)

// A task.run not signed by the pinned gateway key must never execute.
func TestRejectsForgedTaskRun(t *testing.T) {
	gwPub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, dev, _ := ed25519.GenerateKey(rand.Reader)
	_, attacker, _ := ed25519.GenerateKey(rand.Reader)
	c, err := New(Config{Executor: &OpenAIExecutor{ModelList: []string{"m"}}}, Identity{GatewayURL: "http://127.0.0.1:1", Tenant: "acme", DeviceID: "dev_1", PrivateKey: lsp2.B64(dev), GatewayKey: lsp2.B64(gwPub), Issuer: "http://127.0.0.1:1", Audience: lsp2.TokenAudience("http://127.0.0.1:1", "acme")})
	if err != nil {
		t.Fatal(err)
	}
	jws := lsp2.SignTaskRun(attacker, "x", lsp2.TaskRun{TaskID: "task_1", DeviceID: "dev_1", Model: "m", Exp: time.Now().Add(time.Minute).Unix(), Nonce: "n"})
	c.onRun(lsp2.Envelope{Type: lsp2.TypeTaskRun, TaskID: "task_1", JWS: jws})
	if c.Running() != 0 {
		t.Fatal("forged task started")
	}
}

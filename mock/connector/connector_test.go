package connector

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"localsubproxy/mock/v2proto"
)

// A task.run not signed by the pinned gateway key must never execute.
func TestRejectsForgedTaskRun(t *testing.T) {
	gwPub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, attacker, _ := ed25519.GenerateKey(rand.Reader)
	c := New(Config{Models: []string{"m"}})
	c.gw, c.id.DeviceID = gwPub, "dev_1"
	data, _ := json.Marshal(v2proto.TaskRun{DeviceID: "dev_1", Model: "m", Exp: time.Now().Add(time.Minute).Unix(), Nonce: "n"})
	sig := v2proto.B64(ed25519.Sign(attacker, v2proto.TaskRunSigningInput("task_1", data)))
	c.onRun(v2proto.Envelope{Type: v2proto.TypeTaskRun, TaskID: "task_1", Data: data, Sig: sig})
	if len(c.runs) != 0 {
		t.Fatal("forged task started")
	}
}

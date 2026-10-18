package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/tyro668/avora/internal/lmbp"
)

// A task.run not signed by the pinned gateway key must never execute.
func TestRejectsForgedTaskRun(t *testing.T) {
	gwPub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, dev, _ := ed25519.GenerateKey(rand.Reader)
	_, attacker, _ := ed25519.GenerateKey(rand.Reader)
	c, err := New(Config{Executor: &OpenAIExecutor{ModelList: []string{"m"}}}, Identity{GatewayURL: "http://127.0.0.1:1", Tenant: "acme", DeviceID: "dev_1", PrivateKey: lmbp.B64(dev), GatewayKey: lmbp.B64(gwPub), Issuer: "http://127.0.0.1:1", Audience: lmbp.TokenAudience("http://127.0.0.1:1", "acme")})
	if err != nil {
		t.Fatal(err)
	}
	jws := lmbp.SignTaskRun(attacker, "x", lmbp.TaskRun{TaskID: "task_1", DeviceID: "dev_1", Model: "m", Exp: time.Now().Add(time.Minute).Unix(), Nonce: "n"})
	c.onRun(lmbp.Envelope{Type: lmbp.TypeTaskRun, TaskID: "task_1", JWS: jws})
	if c.Running() != 0 {
		t.Fatal("forged task started")
	}
}

// Review #5: SetMaxConcurrency changes the local limit and what every later
// hello/models.update declares; a gateway cap below the declared value
// (known from a lower welcome) still bounds the effective limit.
func TestSetMaxConcurrency(t *testing.T) {
	dev := ed25519.NewKeyFromSeed(make([]byte, 32))
	gwPub, _, _ := ed25519.GenerateKey(nil)
	c, err := New(Config{Executor: &OpenAIExecutor{ModelList: []string{"m"}}, MaxConcurrency: 1}, Identity{GatewayURL: "http://127.0.0.1:1", Tenant: "acme", DeviceID: "dev_1", PrivateKey: lmbp.B64(dev), GatewayKey: lmbp.B64(gwPub), Issuer: "http://127.0.0.1:1", Audience: lmbp.TokenAudience("http://127.0.0.1:1", "acme")})
	if err != nil {
		t.Fatal(err)
	}
	c.SetMaxConcurrency(3)
	if c.declaredConc() != 3 || c.maxConc != 3 {
		t.Fatalf("declared %d effective %d", c.declaredConc(), c.maxConc)
	}
	c.gwConc = 2
	c.SetMaxConcurrency(4)
	if c.declaredConc() != 4 || c.maxConc != 2 {
		t.Fatalf("gateway cap ignored: declared %d effective %d", c.declaredConc(), c.maxConc)
	}
}

// P1-1: the ACK of the last content chunk arrives after the task is marked
// finished but before task.done is enqueued. The task must not be reclaimed
// then; task.done must go out with the next seq and the task is reclaimed
// only once that seq is acked.
func TestAckBetweenFinishedAndDoneKeepsTask(t *testing.T) {
	dev := ed25519.NewKeyFromSeed(make([]byte, 32))
	gwPub, _, _ := ed25519.GenerateKey(nil)
	c, err := New(Config{Executor: &OpenAIExecutor{ModelList: []string{"m"}}}, Identity{GatewayURL: "http://127.0.0.1:1", Tenant: "acme", DeviceID: "dev_1", PrivateKey: lmbp.B64(dev), GatewayKey: lmbp.B64(gwPub), Issuer: "http://127.0.0.1:1", Audience: lmbp.TokenAudience("http://127.0.0.1:1", "acme")})
	if err != nil {
		t.Fatal(err)
	}
	var sent []lmbp.Envelope
	var smu sync.Mutex
	c.out.SetFlow(nil)
	c.out.Attach(func(b []byte) error {
		var e lmbp.Envelope
		_ = json.Unmarshal(b, &e)
		smu.Lock()
		sent = append(sent, e)
		smu.Unlock()
		return nil
	})
	tk := &ctask{id: "task_1", out: c.out, started: true}
	c.tasks["task_1"] = tk
	if _, err := c.out.SendFunc(context.Background(), "task_1", func(int64) lmbp.Envelope {
		return lmbp.Envelope{Type: lmbp.TypeTaskDelta, Data: []byte(`{"content":"hi"}`)}
	}); err != nil {
		t.Fatal(err)
	}
	fh := func(id string) { c.onAck(id, 1) } // ACK of the last chunk lands here
	finishHook.Store(&fh)
	defer finishHook.Store(nil)
	c.finish(tk, &lmbp.TaskDone{FinishReason: "stop"}, nil)
	last := lastSent(t, &smu, &sent)
	if last.Type != lmbp.TypeTaskDone || last.Seq != 2 {
		t.Fatalf("terminal frame %s seq %d (want task.done seq 2)", last.Type, last.Seq)
	}
	for i := 0; i < 100 && func() bool { c.mu.Lock(); defer c.mu.Unlock(); return !tk.terminal }(); i++ {
		time.Sleep(10 * time.Millisecond) // the sender marks it queued right after
	}
	c.mu.Lock()
	_, kept := c.tasks["task_1"]
	c.mu.Unlock()
	if !kept {
		t.Fatal("task reclaimed before task.done was acked")
	}
	c.onAck("task_1", 2)
	c.mu.Lock()
	_, kept = c.tasks["task_1"]
	c.mu.Unlock()
	if kept {
		t.Fatal("task not reclaimed after task.done ack")
	}
}

// Review5 P2-d: the terminal frame waits for flow credit with no deadline
// and goes out (with the next seq) once credit is restored.
func TestTerminalWaitsForCredit(t *testing.T) {
	dev := ed25519.NewKeyFromSeed(make([]byte, 32))
	gwPub, _, _ := ed25519.GenerateKey(nil)
	c, err := New(Config{Executor: &OpenAIExecutor{ModelList: []string{"m"}}}, Identity{GatewayURL: "http://127.0.0.1:1", Tenant: "acme", DeviceID: "dev_1", PrivateKey: lmbp.B64(dev), GatewayKey: lmbp.B64(gwPub), Issuer: "http://127.0.0.1:1", Audience: lmbp.TokenAudience("http://127.0.0.1:1", "acme")})
	if err != nil {
		t.Fatal(err)
	}
	var sent []lmbp.Envelope
	var smu sync.Mutex
	c.out.SetFlow(&lmbp.Flow{TaskWindow: 64, ConnWindow: 1 << 20})
	c.out.Attach(func(b []byte) error {
		var e lmbp.Envelope
		_ = json.Unmarshal(b, &e)
		smu.Lock()
		sent = append(sent, e)
		smu.Unlock()
		return nil
	})
	tk := &ctask{id: "task_1", out: c.out, started: true}
	c.tasks["task_1"] = tk
	// one chunk exhausts the 64-byte task window
	if _, err := c.out.SendFunc(context.Background(), "task_1", func(int64) lmbp.Envelope {
		return lmbp.Envelope{Type: lmbp.TypeTaskDelta, Data: []byte(`{"content":"0123456789012345678901234567890123456789"}`)}
	}); err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	th := func(ctx context.Context) {
		if _, has := ctx.Deadline(); has {
			t.Error("terminal frame wait has a deadline")
		}
		close(waiting)
	}
	terminalCtxHook.Store(&th)
	defer terminalCtxHook.Store(nil)
	c.finish(tk, &lmbp.TaskDone{FinishReason: "stop"}, nil) // returns at once
	<-waiting
	time.Sleep(200 * time.Millisecond)
	smu.Lock()
	n := len(sent)
	smu.Unlock()
	if n != 1 || c.out.Waiting() != 1 {
		t.Fatalf("task.done sent without credit (%d frames, %d waiting)", n, c.out.Waiting())
	}
	c.onAck("task_1", 1) // credit restored
	last := lastSent(t, &smu, &sent)
	for i := 0; i < 100 && func() bool { c.mu.Lock(); defer c.mu.Unlock(); return !tk.terminal }(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if last.Type != lmbp.TypeTaskDone || last.Seq != 2 {
		t.Fatalf("terminal %s seq %d", last.Type, last.Seq)
	}
	c.onAck("task_1", 2)
	c.mu.Lock()
	_, kept := c.tasks["task_1"]
	c.mu.Unlock()
	if kept {
		t.Fatal("task not reclaimed")
	}
}

// ...and a permanent stop releases a terminal frame still waiting.
func TestTerminalWaitEndsOnPermanentStop(t *testing.T) {
	dev := ed25519.NewKeyFromSeed(make([]byte, 32))
	gwPub, _, _ := ed25519.GenerateKey(nil)
	c, _ := New(Config{Executor: &OpenAIExecutor{ModelList: []string{"m"}}}, Identity{GatewayURL: "http://127.0.0.1:1", Tenant: "acme", DeviceID: "dev_1", PrivateKey: lmbp.B64(dev), GatewayKey: lmbp.B64(gwPub), Issuer: "http://127.0.0.1:1", Audience: lmbp.TokenAudience("http://127.0.0.1:1", "acme")})
	c.out.SetFlow(&lmbp.Flow{TaskWindow: 64, ConnWindow: 1 << 20})
	c.out.Attach(func([]byte) error { return nil })
	tk := &ctask{id: "task_1", out: c.out, started: true}
	c.tasks["task_1"] = tk
	_, _ = c.out.SendFunc(context.Background(), "task_1", func(int64) lmbp.Envelope {
		return lmbp.Envelope{Type: lmbp.TypeTaskDelta, Data: []byte(`{"content":"0123456789012345678901234567890123456789"}`)}
	})
	c.finish(tk, &lmbp.TaskDone{FinishReason: "stop"}, nil)
	for i := 0; c.out.Waiting() == 0; i++ {
		if i > 500 {
			t.Fatal("terminal frame never queued")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = c.Run(ctx, nil) // returns at once: permanent stop
	for i := 0; c.out.Waiting() != 0; i++ {
		if i > 500 {
			t.Fatal("terminal wait not released by permanent stop")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// lastSent waits until the last captured frame is a terminal one.
func lastSent(t *testing.T, mu *sync.Mutex, sent *[]lmbp.Envelope) lmbp.Envelope {
	t.Helper()
	for i := 0; i < 500; i++ {
		mu.Lock()
		if n := len(*sent); n > 0 && ((*sent)[n-1].Type == lmbp.TypeTaskDone || (*sent)[n-1].Type == lmbp.TypeTaskError) {
			e := (*sent)[n-1]
			mu.Unlock()
			return e
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no terminal frame sent")
	return lmbp.Envelope{}
}

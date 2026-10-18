package client_test

import (
	"context"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/client"
)

type swapExec struct {
	mu     sync.Mutex
	models []string
}

func (e *swapExec) Models() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.models...)
}
func (e *swapExec) set(m ...string) { e.mu.Lock(); e.models = m; e.mu.Unlock() }
func (e *swapExec) Execute(context.Context, lmbp.TaskRun, func(lmbp.TaskDelta) error) (*lmbp.TaskDone, *lmbp.ErrorBody) {
	return &lmbp.TaskDone{FinishReason: "stop"}, nil
}

// audit3 P2-2 (client): a models.update built on session 1 is delayed;
// the connection drops and session 2 says hello with the newer model list.
// The stale update must not be written on session 2 (it would overwrite
// that hello); it is bound to its session and rechecked at write time.
func TestModelsChangedBoundToSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	g := newGW(t, ctx)
	ex := &swapExec{models: []string{"model-a"}}
	c, err := client.New(client.Config{Executor: ex, MaxConcurrency: 1, Logger: log.New(io.Discard, "", 0)}, g.id)
	if err != nil {
		t.Fatal(err)
	}
	rctx, rcancel := context.WithCancel(ctx)
	defer rcancel()
	ready := make(chan error, 1)
	go c.Run(rctx, ready)
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	hello2 := make(chan struct{})
	var hOnce sync.Once
	client.SetHandshakeHook(func() { hOnce.Do(func() { close(hello2) }) })
	defer client.SetHandshakeHook(nil)
	var once sync.Once
	client.SetAnnounceHook(func(b []byte) {
		once.Do(func() { // the stale update (model-a), built on session 1
			ex.set("model-b") // newer list: session 2's hello carries it
			c.Disconnect()
			<-hello2
			time.Sleep(500 * time.Millisecond) // session 2 is welcomed meanwhile
		})
	})
	defer client.SetAnnounceHook(nil)
	c.ModelsChanged()
	time.Sleep(500 * time.Millisecond)
	for i := 0; ; i++ {
		ds := g.call("GET", "/lmbp/v1/tenant/devices", nil)
		if strings.Contains(ds, "model-b") && !strings.Contains(ds, "model-a") {
			return
		}
		if i > 40 {
			t.Fatalf("gateway holds a stale model list after reconnect: %s", ds)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

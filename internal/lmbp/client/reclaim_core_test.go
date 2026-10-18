package client_test

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/tyro668/avora/internal/lmbp"
	"github.com/tyro668/avora/internal/lmbp/client"
	"github.com/tyro668/avora/internal/lmbp/gateway"
)

type chunkExec struct{}

func (chunkExec) Models() []string { return []string{"m"} }
func (chunkExec) Execute(ctx context.Context, run lmbp.TaskRun, emit func(lmbp.TaskDelta) error) (*lmbp.TaskDone, *lmbp.ErrorBody) {
	for _, s := range []string{"a", "b", "c"} {
		if emit(lmbp.TaskDelta{Text: s}) != nil {
			break
		}
	}
	return &lmbp.TaskDone{FinishReason: "stop"}, nil
}

// audit2 #5: ACK is Core. Against a Core-only gateway (no flow, no resume)
// the device still gets every data frame acknowledged and reclaims finished
// tasks: no task state or retransmit buffer is left behind.
func TestCoreOnlyAckReclaimsTasks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	g := newGWCfg(t, ctx, func(c *gateway.Config) { c.CoreOnly = true })
	c, err := client.New(client.Config{Executor: chunkExec{}, MaxConcurrency: 2, Logger: log.New(io.Discard, "", 0)}, g.id)
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
	if dev := g.call("GET", "/lmbp/v1/tenant/devices", nil); strings.Contains(dev, `"flow"`) || strings.Contains(dev, `"resume"`) {
		t.Fatalf("Core-only gateway negotiated an extension: %s", dev)
	}
	for i := 0; i < 5; i++ {
		out := g.call("POST", "/v1/chat/completions", map[string]any{"model": g.id.DeviceID + "/m", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
		if !strings.Contains(out, `"content":"abc"`) {
			t.Fatalf("task %d: %s", i, out)
		}
	}
	var tasks, frames int
	for i := 0; i < 100; i++ {
		if tasks, frames = c.Pending(); tasks == 0 && frames == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("after 5 finished tasks without flow the device still holds %d tasks and %d unacknowledged frames", tasks, frames)
}

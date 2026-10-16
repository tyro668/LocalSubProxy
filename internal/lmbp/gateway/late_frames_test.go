package gateway

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
)

// Review7 #1: the gateway ends tasks locally (here: the HTTP client goes
// away) while the device is still streaming. The device's late frames,
// including its terminal frame, must be ACKed so its flow credit comes
// back; after N cancels the device's window is empty again.
func TestLocalCancelsReleaseDeviceCredit(t *testing.T) {
	r := newRig(t, lmbp.Flow{TaskWindow: lmbp.DefaultTaskWindow, ConnWindow: lmbp.DefaultConnWindow}, 40, 20*time.Millisecond)
	for i := 0; i < 6; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		req := r.req("POST", "/v1/chat/completions", map[string]any{"model": r.model("slow"), "stream": true,
			"messages": []map[string]string{{"role": "user", "content": "go"}}}, nil).WithContext(ctx)
		resp, err := httpc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 512)
		_, _ = io.ReadAtLeast(resp.Body, buf, 256) // the device is streaming
		cancel()                                   // client gone: gateway ends the task locally
		resp.Body.Close()
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		f, b := r.dev.OutboxUsage()
		if f == 0 && b == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("device credit never came back: %d frames / %d bytes unacked", f, b)
		}
		time.Sleep(50 * time.Millisecond)
	}
	r.g.mu.Lock()
	ds := r.g.devs[r.devID]
	r.g.mu.Unlock()
	ds.mu.Lock()
	n := len(ds.reclaimed)
	ds.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d late-frame records left after the device finished", n)
	}
}

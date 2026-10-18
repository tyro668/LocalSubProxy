package gateway

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/tyro668/avora/internal/lmbp"
)

// Review8 P2-a: a local finish (client cancel) and the device's terminal
// frame interleave exactly in the window of the local finish transition:
// the device frame (held at the read loop's hand-over) is received by
// the real read loop and fully handled
// while finishWith is between "task ended" and "late-frame cleanup
// registered". The device's credit must still come back in full, without
// a reconnect, and no cleanup record may be left behind.
func TestLocalFinishRacesDeviceTerminal(t *testing.T) {
	r := newRig(t, lmbp.Flow{TaskWindow: lmbp.DefaultTaskWindow, ConnWindow: lmbp.DefaultConnWindow}, 40, 20*time.Millisecond)
	var once sync.Once
	lateRead := make(chan struct{})
	handled := make(chan struct{})
	h := func(in lmbp.Envelope) func() {
		if !isTerminal(in.Type) {
			return nil
		}
		<-lateRead // the read loop holds the device terminal until finish is mid-transition
		return func() {
			select {
			case <-handled:
			default:
				close(handled)
			}
		}
	}
	deviceDataHook.Store(&h)
	defer deviceDataHook.Store(nil)
	fired := false
	transitioned := make(chan struct{})
	raceHook = func(gt *gtask, point string) {
		switch point {
		case "finish-transition":
			once.Do(func() {
				fired = true
				close(lateRead)
				// the gateway has decided locally and already sent
				// task.cancel; hold the transition until the device's
				// terminal frame went through the read loop
				select {
				case <-handled:
				case <-time.After(10 * time.Second):
					t.Error("device terminal frame never arrived")
				}
				close(transitioned)
			})
		}
	}
	defer func() { raceHook = nil }()
	ctx, cancel := context.WithCancel(context.Background())
	req := r.req("POST", "/v1/chat/completions", map[string]any{"model": r.model("slow"), "stream": true,
		"messages": []map[string]string{{"role": "user", "content": "go"}}}, nil).WithContext(ctx)
	resp, err := httpc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 512)
	_, _ = io.ReadAtLeast(resp.Body, buf, 256)
	cancel()
	resp.Body.Close()
	select {
	case <-transitioned:
	case <-time.After(15 * time.Second):
		t.Fatal("local finish transition never ran")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		f, b := r.dev.OutboxUsage()
		if f == 0 && b == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("device credit not returned after the race: %d frames / %d bytes unacked", f, b)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !fired {
		t.Fatal("race window was not exercised")
	}
	r.g.mu.Lock()
	ds := r.g.devs[r.devID]
	r.g.mu.Unlock()
	ds.mu.Lock()
	n := len(ds.reclaimed)
	ds.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d late-frame records left", n)
	}
}

package gateway

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
)

// Review7 #10: another path ends a queued task right before the queue exit
// decides; the caller must get that recorded outcome, not a made-up 429.
func TestQueueExitReportsWinningOutcome(t *testing.T) {
	r := newRig(t, lmbp.Flow{}, 8, 250*time.Millisecond)
	r.g.cfg.QueueTimeout = 300 * time.Millisecond
	var once sync.Once
	raceHook = func(tk *gtask, point string) {
		if point == "queue-exit" {
			once.Do(func() { tk.failLocal(lmbp.ErrCancelled, "cancelled by winner", false) })
		}
	}
	defer func() { raceHook = nil }()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); idemChat(t, r, "slow", "x", "") }()
	}
	time.Sleep(300 * time.Millisecond)
	first := idemChat(t, r, "echo", "busy", "k-win")
	again := idemChat(t, r, "echo", "busy", "k-win")
	wg.Wait()
	if first.code == http.StatusTooManyRequests || strings.Contains(first.body, lmbp.ErrDeviceBusy) || !strings.Contains(first.body, "cancelled by winner") {
		t.Fatalf("first did not report the winner: %d %s", first.code, first.body)
	}
	if again.task != first.task || !strings.Contains(again.body, "cancelled by winner") {
		t.Fatalf("replay differs: %d %s", again.code, again.body)
	}
}

// Review7 #10: a caller that disconnects while queued records CANCELLED
// (not DEVICE_BUSY), and an idempotent replay sees exactly that.
func TestQueuedDisconnectRecordsCancelled(t *testing.T) {
	r := newRig(t, lmbp.Flow{}, 8, 250*time.Millisecond)
	r.g.cfg.QueueTimeout = 5 * time.Second
	exited := make(chan struct{})
	var once sync.Once
	raceHook = func(tk *gtask, point string) {
		if point == "queue-exit" {
			once.Do(func() { close(exited) })
		}
	}
	defer func() { raceHook = nil }()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); idemChat(t, r, "slow", "x", "") }()
	}
	time.Sleep(300 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	req := r.req("POST", "/v1/chat/completions", map[string]any{"model": "echo", "messages": user("gone")}, map[string]string{"Idempotency-Key": "k-gone"}).WithContext(ctx)
	go func() {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("queued request did not exit on disconnect")
	}
	time.Sleep(50 * time.Millisecond)
	wg.Wait()
	again := idemChat(t, r, "echo", "gone", "k-gone")
	if !strings.Contains(again.body, lmbp.ErrCancelled) || strings.Contains(again.body, lmbp.ErrDeviceBusy) {
		t.Fatalf("replay after disconnect: %d %s", again.code, again.body)
	}
}

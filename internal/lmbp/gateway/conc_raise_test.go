package gateway

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
)

// Audit2 P2-2: raising max_concurrency (models.update) hands the new slots
// to queued requests at once, in FIFO order. conc=1, A running, B queued,
// raise to 2: B starts immediately and never gets DEVICE_BUSY.
func TestRaisedConcurrencyWakesQueuedRequests(t *testing.T) {
	r := newRig(t, lmbp.Flow{}, 3000, 10*time.Millisecond) // "slow" runs ~30s, longer than QueueTimeout
	r.g.mu.Lock()
	ds := r.g.devs[r.devID]
	r.g.mu.Unlock()
	waitFor := func(what string, ok func() bool) {
		for end := time.Now().Add(10 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	r.dev.SetMaxConcurrency(1)
	waitFor("conc 1", func() bool { return ds.concurrency() == 1 })

	actx, acancel := context.WithCancel(context.Background())
	defer acancel()
	go func() {
		req := r.req("POST", "/v1/chat/completions", map[string]any{"model": r.model("slow"), "messages": []map[string]string{{"role": "user", "content": "a"}}}, nil).WithContext(actx)
		if resp, err := httpc.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	waitFor("A running", func() bool { ds.mu.Lock(); defer ds.mu.Unlock(); return ds.slots == 1 })

	type result struct {
		status int
		body   string
	}
	done := make(chan result, 1)
	go func() {
		resp, err := httpc.Do(r.req("POST", "/v1/chat/completions", map[string]any{"model": r.model("echo"), "messages": []map[string]string{{"role": "user", "content": "b"}}}, nil))
		if err != nil {
			done <- result{0, err.Error()}
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		done <- result{resp.StatusCode, string(b)}
	}()
	waitFor("B queued", func() bool { ds.mu.Lock(); defer ds.mu.Unlock(); return len(ds.slotWait) == 1 })

	raised := time.Now()
	r.dev.SetMaxConcurrency(2)
	select {
	case res := <-done:
		if res.status != http.StatusOK {
			t.Fatalf("B after raise: %d %s", res.status, res.body)
		}
		if el := time.Since(raised); el > 3*time.Second {
			t.Fatalf("B started %v after the raise", el)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("B still queued 20s after max_concurrency was raised to 2")
	}
}

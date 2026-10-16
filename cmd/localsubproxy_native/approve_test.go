package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp/client"
)

// Review4 C: a network failure during pair_approve keeps the consent
// request retryable (same pair_id); the retry pairs normally. While the
// approve request is in flight, status answers promptly.
func TestPairApproveRetryAfterNetworkError(t *testing.T) {
	s := newTestSaaS(t, "acme")
	startKernel(t, dir(t))
	defer must(t, map[string]any{"action": "stop"})
	p := must(t, map[string]any{"action": "pair_start", "remote_url": s.remote})
	orig := pairApprove
	inFlight, release := make(chan struct{}), make(chan struct{})
	pairApprove = func(ctx context.Context, r *client.PairRequest) (*client.Pairing, error) {
		close(inFlight)
		<-release
		return nil, errors.New("device authorization failed: i/o timeout (injected)")
	}
	done := make(chan reply)
	go func() {
		done <- do(t, map[string]any{"action": "pair_approve", "pair_id": p["pair_id"], "allow": []string{"*"}})
	}()
	<-inFlight
	t0 := time.Now()
	must(t, map[string]any{"action": "status"})
	if d := time.Since(t0); d > 500*time.Millisecond {
		t.Fatalf("status blocked during approve: %v", d)
	}
	if r := do(t, map[string]any{"action": "pair_approve", "pair_id": p["pair_id"], "allow": []string{"*"}}); r.OK {
		t.Fatal("second concurrent approve accepted")
	}
	close(release)
	r := <-done
	pairApprove = orig
	if r.OK || !strings.Contains(r.Error, "retry") {
		t.Fatalf("first approve: %+v", r)
	}
	st := must(t, map[string]any{"action": "pair_status", "pair_id": p["pair_id"]})
	if st["state"] != "consent" || st["consent"] == nil {
		t.Fatalf("not retryable: %v", st)
	}
	a := must(t, map[string]any{"action": "pair_approve", "pair_id": p["pair_id"], "allow": []string{"*"}})
	s.approve(t, a["user_code"].(string))
	for i := 0; i < 100; i++ {
		if st = must(t, map[string]any{"action": "pair_status", "pair_id": p["pair_id"]}); st["state"] != "pending" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if st["state"] != "paired" {
		t.Fatalf("retry did not pair: %v", st)
	}
}

func dir(t *testing.T) string { return t.TempDir() }

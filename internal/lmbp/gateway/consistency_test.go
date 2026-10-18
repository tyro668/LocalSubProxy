package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/tyro668/avora/internal/lmbp"
)

// SDK consistency review (2026-10, main@52f48ae), P2-2: a model the named,
// online device does not declare was answered 503 DEVICE_OFFLINE (retryable)
// by every implementation; spec §6.6 says 404 MODEL_NOT_FOUND.
func TestUndeclaredModelOnOnlineDeviceIs404(t *testing.T) {
	r := newRig(t, lmbp.Flow{}, 0, 0)
	code, m := r.api(t, "POST", "/v1/chat/completions", map[string]any{"model": r.model("nope"), "messages": user("x")}, nil)
	e, _ := m["error"].(map[string]any)
	if code != 404 || e["code"] != lmbp.ErrModelNotFound {
		t.Fatalf("chat: %d %v", code, m)
	}
	code, m = r.api(t, "POST", "/v1/messages", map[string]any{"model": r.model("nope"), "max_tokens": 5, "messages": user("x")}, map[string]string{"Anthropic-Version": "2023-06-01"})
	e, _ = m["error"].(map[string]any)
	if code != 404 || e["type"] != "not_found_error" {
		t.Fatalf("messages: %d %v", code, m)
	}
	// The discovery documents keep their own caching policy (§6.1).
	resp := doReq(t, r.req("GET", "/.well-known/jwks.json", nil, nil))
	resp.Body.Close()
	if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=300" {
		t.Fatalf("jwks Cache-Control %q", cc)
	}
}

// P2-1: the retention pass removes a finished task from the store with its
// idempotency record AND from the in-memory registry, so the replay of the
// same key and body runs again; the pass runs every RetentionEvery.
func TestRetentionPurgeReexecutesKeyedReplay(t *testing.T) {
	r := newRig(t, lmbp.Flow{}, 0, 0)
	a := idemChat(t, r, r.model("echo"), "one two", "k-retention")
	b := idemChat(t, r, r.model("echo"), "one two", "k-retention")
	if a.code != 200 || b.task != a.task {
		t.Fatalf("immediate replay: %d %s / %s", a.code, a.task, b.task)
	}
	if n := r.g.Purge(context.Background(), time.Now()); n != 0 {
		t.Fatalf("purged %d tasks before the retention elapsed", n)
	}
	if n := r.g.Purge(context.Background(), time.Now().Add(7*24*time.Hour+time.Minute)); n != 1 {
		t.Fatalf("purge after the retention: n=%d", n)
	}
	c := idemChat(t, r, r.model("echo"), "one two", "k-retention")
	if c.code != 200 || c.task == a.task || r.exec.count(c.task) != 1 {
		t.Fatalf("replay after the purge did not run again: %d task %s (first %s)", c.code, c.task, a.task)
	}
	for ret, want := range map[time.Duration]time.Duration{7 * 24 * time.Hour: time.Hour, 3 * time.Second: 1500 * time.Millisecond, time.Second: time.Second} {
		if got := RetentionEvery(ret); got != want {
			t.Fatalf("RetentionEvery(%v) = %v, want %v", ret, got, want)
		}
	}
}

package gateway

import (
	"net/http"
	"testing"
	"time"

	"github.com/tyro668/avora/internal/lmbp"
)

// Review9 P2-6: during the handshake the device session (and its models)
// became routable before the welcome was sent and the sender attached. A
// request arriving in that window created a task whose task.run sat in the
// outbox: AttachOwned paused it for retransmission, but it was not in the
// resume list (and a first connect never retransmits), so the request hung
// until its timeout. A request must only be routed once welcome, sender and
// resume state are ready; in the window it sees no device (or is served).
func TestRequestDuringHandshakeIsNotStuck(t *testing.T) {
	r := newRig(t, lmbp.Flow{}, 0, 0)
	type res struct {
		code int
		err  error
	}
	done := make(chan res, 1)
	fired := make(chan struct{})
	handshakeHook = func(id string) {
		if id != r.devID {
			return
		}
		select {
		case <-fired:
			return
		default:
		}
		close(fired)
		go func() {
			req := r.req("POST", "/v1/chat/completions", map[string]any{"model": r.model("echo"), "messages": user("in the window")}, nil)
			c := &http.Client{Timeout: 8 * time.Second}
			resp, err := c.Do(req)
			if err != nil {
				done <- res{0, err}
				return
			}
			buf := make([]byte, 4096)
			for {
				if _, err := resp.Body.Read(buf); err != nil {
					break
				}
			}
			resp.Body.Close()
			done <- res{resp.StatusCode, nil}
		}()
		// Hold the handshake until task.run was queued (or the request refused).
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			r.g.mu.Lock()
			ds := r.g.devs[id]
			r.g.mu.Unlock()
			n := 0
			if ds != nil {
				n = len(ds.out.Tasks()) // task.run already queued in the outbox
			}
			if n > 0 || len(done) > 0 {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	defer func() { handshakeHook = nil }()
	// Drop the connection: the device reconnects (resume) and the hook fires
	// inside the new handshake.
	r.g.mu.Lock()
	ds0 := r.g.devs[r.devID]
	r.g.mu.Unlock()
	ds0.mu.Lock()
	c0 := ds0.conn
	ds0.mu.Unlock()
	_ = c0.Close()
	select {
	case <-fired:
	case <-time.After(10 * time.Second):
		t.Fatal("device did not reconnect")
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("request in the handshake window hung: %v", got.err)
		}
		t.Logf("request in the handshake window: HTTP %d", got.code)
		if got.code != 200 && got.code != 503 {
			t.Fatalf("request in the handshake window: HTTP %d", got.code)
		}
	case <-time.After(9 * time.Second):
		t.Fatal("request in the handshake window hung")
	}
	afterHandshakeOK(t, r)
}

// afterHandshakeOK: once the handshake completed the device serves requests.
func afterHandshakeOK(t *testing.T, r *rig) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		a := idemChat(t, r, "echo", "after", "")
		if a.code == 200 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("device not usable after the handshake: %d %s", a.code, a.body)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

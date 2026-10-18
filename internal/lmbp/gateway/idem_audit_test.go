package gateway

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyro668/avora/internal/lmbp"
	"github.com/tyro668/avora/internal/lmbp/store"
)

// gateStore blocks CreateTask until released (to hit the window between the
// idempotency record and the task row).
type gateStore struct {
	store.Store
	entered, release chan struct{}
	once             sync.Once
}

func (g *gateStore) CreateTask(ctx context.Context, t store.Task, i *store.Idempotency) error {
	g.once.Do(func() { close(g.entered); <-g.release })
	return g.Store.CreateTask(ctx, t, i)
}

type result struct {
	code       int
	task, body string
}

func idemChat(t *testing.T, r *rig, model, text, key string) result {
	resp := doReq(t, r.req("POST", "/v1/chat/completions", map[string]any{"model": model, "messages": user(text)}, map[string]string{"Idempotency-Key": key}))
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return result{resp.StatusCode, resp.Header.Get("X-Task-Id"), b.String()}
}

func both(t *testing.T, a, b func() result) (result, result) {
	var ra, rb result
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); ra = a() }()
	time.Sleep(150 * time.Millisecond)
	rb = b()
	wg.Wait()
	return ra, rb
}

func sameRun(t *testing.T, r *rig, a, b result) {
	t.Helper()
	if a.code != 200 || b.code != 200 || a.task == "" || a.task != b.task || a.body != b.body {
		t.Fatalf("duplicate not attached to the same task:\n first  %d %s %s\n second %d %s %s", a.code, a.task, a.body, b.code, b.task, b.body)
	}
	if n := r.exec.count(a.task); n != 1 {
		t.Fatalf("executed %d times", n)
	}
}

// Reliability 2a: a duplicate arriving between the idempotency record and
// the task row must attach to the pending task (was: 404 task no longer available).
func TestIdempotentDuplicateDuringCreate(t *testing.T) {
	gs := &gateStore{entered: make(chan struct{}), release: make(chan struct{})}
	r := newRigWith(t, lmbp.Flow{}, 0, 0, func(s store.Store) store.Store { gs.Store = s; return gs })
	a, b := both(t, func() result { return idemChat(t, r, "echo", "one two", "k-create") }, func() result {
		<-gs.entered
		time.AfterFunc(300*time.Millisecond, func() { close(gs.release) })
		return idemChat(t, r, "echo", "one two", "k-create")
	})
	sameRun(t, r, a, b)
}

// Reliability 2b: a duplicate arriving while the task waits for a device
// slot must attach to it (was: OUTCOME_UNKNOWN from an event-less task row).
func TestIdempotentDuplicateWhileQueued(t *testing.T) {
	r := newRig(t, lmbp.Flow{}, 4, 250*time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ { // occupy every slot for ~1s
		wg.Add(1)
		go func() { defer wg.Done(); idemChat(t, r, "slow", "x", "") }()
	}
	time.Sleep(300 * time.Millisecond)
	a, b := both(t, func() result { return idemChat(t, r, "echo", "queued call", "k-queue") }, func() result { return idemChat(t, r, "echo", "queued call", "k-queue") })
	wg.Wait()
	sameRun(t, r, a, b)
}

// Reliability 2c: when queueing fails the outcome is recorded, so a replay
// with the same key gets the same error instead of OUTCOME_UNKNOWN.
func TestIdempotentQueueFailureReplays(t *testing.T) {
	r := newRig(t, lmbp.Flow{}, 8, 250*time.Millisecond)
	r.g.cfg.QueueTimeout = 300 * time.Millisecond
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); idemChat(t, r, "slow", "x", "") }()
	}
	time.Sleep(300 * time.Millisecond)
	first := idemChat(t, r, "echo", "busy", "k-busy")
	again := idemChat(t, r, "echo", "busy", "k-busy")
	wg.Wait()
	if first.code != http.StatusTooManyRequests || !strings.Contains(first.body, lmbp.ErrDeviceBusy) {
		t.Fatalf("first: %d %s", first.code, first.body)
	}
	if again.code != first.code || !strings.Contains(again.body, lmbp.ErrDeviceBusy) || again.task != first.task {
		t.Fatalf("replay after queue failure: %d %s %s (first task %s)", again.code, again.task, again.body, first.task)
	}
}

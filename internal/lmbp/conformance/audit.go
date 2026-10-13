package conformance

// Z*: regression cases from the 2026-10 security/robustness audit. Each check
// pins one finding as black-box behaviour every implementation must share.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
)

func (r *runner) runAudit(ctx context.Context, dev *extDev, full func(string) string, mTools string) {
	r.auditIdempotency(ctx, dev, full(mTools))
}

// Z1: idempotency records are scoped to (tenant, end user, key) without any
// ambiguous concatenation: (user U, key "x|y") and (user "U|x", key "y") are
// different records, and a replay never reattaches another user's task.
func (r *runner) auditIdempotency(ctx context.Context, dev *extDev, model string) {
	body := map[string]any{"model": model, "stream": false, "messages": []any{map[string]any{"role": "user", "content": "idem"}}}
	type res struct {
		st   int
		body []byte
	}
	ch := make(chan res, 1)
	go func() {
		s, _, b := r.chatRaw(ctx, body, map[string]string{"Idempotency-Key": "x|y"})
		ch <- res{s, b}
	}()
	f, _, err := dev.await(lmbp.TypeTaskRun, nil)
	if err != nil {
		r.check("Z1", "idempotency key scoping", err)
		return
	}
	_ = dev.send(lmbp.TypeTaskDelta, f.TaskID, 1, lmbp.TaskDelta{Text: "owner-secret"})
	_ = dev.send(lmbp.TypeTaskDone, f.TaskID, 2, lmbp.TaskDone{FinishReason: "stop"})
	first := <-ch
	st, _, b := r.chatRaw(ctx, body, map[string]string{"Idempotency-Key": "y", "X-End-User": r.cfg.EndUser + "|x"})
	r.check("Z1", "(user U, Idempotency-Key \"x|y\") and (user \"U|x\", key \"y\") do not collide: the second user cannot replay the first user's task",
		want(first.st == 200 && strings.Contains(string(first.body), "owner-secret") && st != 200 && !strings.Contains(string(b), "owner-secret"),
			"first %d; second %d %s", first.st, st, b))
	st, _, b = r.chatRaw(ctx, body, map[string]string{"Idempotency-Key": "x|y"})
	r.check("Z1b", "the owner's replay with the same key and body still returns the original task",
		want(st == 200 && strings.Contains(string(b), "owner-secret"), "status %d %s", st, b))
}

// Z2: the gateway never has more unacknowledged bytes in flight than the
// device's advertised windows, also when several tasks compete for the
// connection window and wake together on an ack (audit finding 2). The
// device reconnects with task = conn window = 262,144 bytes (>= max frame),
// acks in 30ms batches, and receives four concurrent 512 KiB image uploads as blob.chunk
// frames. It returns the new session for the remaining checks.
func (r *runner) auditFlow(ctx context.Context, key ed25519.PrivateKey, at string, hello lmbp.Hello, model string) *extDev {
	const window = 262144
	hello.Flow = &lmbp.Flow{TaskWindow: window, ConnWindow: window}
	hello.Proof = lmbp.SignProof(key, r.devID, lmbp.ProofHello, at, time.Now())
	dev, _, err := r.dialExt(at, hello)
	if !r.check("Z2", "flow windows hold under concurrency", err) {
		return nil
	}
	dev.mu.Lock()
	dev.batchAcks, dev.inflight = true, map[string]int{}
	dev.mu.Unlock()
	stop := make(chan struct{})
	go dev.ackLoop(stop)
	const n = 4
	img := make([]byte, 512<<10)
	_, _ = rand.Read(img)
	uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString(img)
	sts := make(chan int, n)
	for i := 0; i < n; i++ {
		go func() {
			st, _, _ := r.chatRaw(ctx, map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": fmt.Sprint("flow ", i)}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": uri}}}}}}, nil)
			sts <- st
		}()
	}
	runs, open, got := map[string]bool{}, map[string]bool{}, 0
	tm := time.After(r.cfg.Timeout)
	for err == nil && (len(runs) < n || len(open) > 0) {
		select {
		case f, ok := <-dev.frames:
			if !ok {
				err = fmt.Errorf("session closed")
				break
			}
			switch f.Type {
			case lmbp.TypeTaskRun:
				run, verr := lmbp.VerifyTaskRun(r.gwKey, f.TaskID, f.JWS)
				if verr != nil {
					err = verr
					break
				}
				runs[f.TaskID] = true
				for _, b := range run.Blobs {
					open[f.TaskID+"/"+b.ID] = true
				}
			case lmbp.TypeBlobChunk:
				var c lmbp.BlobChunk
				_ = json.Unmarshal(f.Data, &c)
				p, _ := lmbp.UnB64(c.Data)
				got += len(p)
				if c.Last {
					delete(open, f.TaskID+"/"+c.BlobID)
				}
			}
		case <-tm:
			err = fmt.Errorf("timeout: %d runs, %d blobs open, %d bytes", len(runs), len(open), got)
		}
	}
	for id := range runs {
		_ = dev.send(lmbp.TypeTaskDone, id, 1, lmbp.TaskDone{FinishReason: "stop"})
	}
	ok := 0
	for i := 0; i < len(runs); i++ {
		if <-sts == 200 {
			ok++
		}
	}
	close(stop)
	dev.mu.Lock()
	pt, pc := dev.peakTask, dev.peakConn
	dev.batchAcks = false
	dev.mu.Unlock()
	r.check("Z2", fmt.Sprintf("with %d tasks competing for a %d-byte window, unacknowledged gateway bytes never exceed the task or connection window", n, window),
		want(err == nil && ok == n && got == n*len(img) && pt <= window && pc <= window,
			"err %v ok %d/%d blob bytes %d; peak unacked per task %d, per connection %d (window %d)", err, ok, n, got, pt, pc, window))
	return dev
}
